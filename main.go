package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

//go:embed index.html
var indexHTML []byte

type Config struct {
	Host       string           `json:"host"`
	Port       int              `json:"port"`
	User       string           `json:"user"`
	KeyFile    string           `json:"key_file"`
	Controller ControllerConfig `json:"controller"`
}

// The deadline covers TCP, SSH handshake, and command execution, not just dialing.
func runSSHCommand(ctx context.Context, cfg Config, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	key, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return "", fmt.Errorf("read SSH key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("parse SSH key: %w", err)
	}
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	// Preserve the existing SSH trust policy. Restrict this service to the trusted LAN.
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: cfg.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		return "", err
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr
	if err = session.Run(cmd); err != nil {
		return "", fmt.Errorf("SSH command failed: %w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}

func hardwareIO(cfg Config) ControllerIO {
	return ControllerIO{
		Telemetry: func(ctx context.Context) (Telemetry, error) { return readTelemetry(ctx, cfg) },
		Ambient: func(ctx context.Context) (float64, error) {
			return ReadAmbient(ctx, http.DefaultClient, cfg.Controller.HomebridgeURL, cfg.Controller.Accessory)
		},
		Disks: func(ctx context.Context) ([]DiskReading, error) {
			data, err := runSSHCommand(ctx, cfg, "sudo -n /usr/local/sbin/unas-disk-temperatures")
			if err != nil {
				return nil, err
			}
			return ParseDisks([]byte(data), cfg.Controller.Disks)
		},
		ReadPWM: func(ctx context.Context) ([2]int, error) {
			var pwm [2]int
			out, err := runSSHCommand(ctx, cfg, "cat /sys/class/hwmon/hwmon0/pwm1 /sys/class/hwmon/hwmon0/pwm2")
			if err != nil {
				return pwm, err
			}
			fields := strings.Fields(out)
			if len(fields) != 2 {
				return pwm, errors.New("expected two PWM values")
			}
			for i, f := range fields {
				n, err := strconv.Atoi(f)
				if err != nil || n < 0 || n > 255 {
					return pwm, errors.New("invalid PWM value")
				}
				pwm[i] = n
			}
			return pwm, nil
		},
		WritePWM: func(ctx context.Context, pwm int) error {
			if pwm < 0 || pwm > 255 {
				return errors.New("PWM out of range")
			}
			_, err := runSSHCommand(ctx, cfg, fmt.Sprintf("echo %d > /sys/class/hwmon/hwmon0/pwm1 && echo %d > /sys/class/hwmon/hwmon0/pwm2", pwm, pwm))
			return err
		},
	}
}

func decodeRequest(w http.ResponseWriter, r *http.Request, v any) error {
	defer r.Body.Close()
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func newMux(c *Controller, sensors func(context.Context) (string, error)) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/sensors", func(w http.ResponseWriter, r *http.Request) {
		s, err := sensors(r.Context())
		if err != nil {
			log.Printf("Read sensors: %v", err)
			http.Error(w, "Failed to get sensors output", 500)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(s))
	})
	mux.HandleFunc("GET /api/controller", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, c.Status()) })
	mux.HandleFunc("GET /api/experiment/{file}", c.experimentDownload)
	mux.HandleFunc("POST /api/mode", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode string `json:"mode"`
		}
		if decodeRequest(w, r, &req) != nil || (req.Mode != "auto" && req.Mode != "manual") {
			http.Error(w, "Expected mode auto or manual", 400)
			return
		}
		if err := c.SetMode(r.Context(), req.Mode, time.Now()); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		jsonResponse(w, c.Status())
	})
	mux.HandleFunc("GET /api/fan", func(w http.ResponseWriter, r *http.Request) {
		c.op.Lock()
		pwm, err := c.io.ReadPWM(r.Context())
		c.op.Unlock()
		if err != nil {
			http.Error(w, "Failed to get fan speed", 500)
			return
		}
		jsonResponse(w, struct {
			Speed int `json:"speed"`
		}{pwm[0]})
	})
	mux.HandleFunc("POST /api/fan", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Speed *int `json:"speed"`
		}
		if decodeRequest(w, r, &req) != nil || req.Speed == nil || *req.Speed < 0 || *req.Speed > 255 {
			http.Error(w, "Speed must be an integer between 0 and 255", 400)
			return
		}
		if err := c.SetManual(r.Context(), *req.Speed, time.Now()); err != nil {
			log.Printf("Manual fan update: %v", err)
			http.Error(w, err.Error(), 500)
			return
		}
		jsonResponse(w, map[string]string{"status": "success", "message": "Fan speed updated"})
	})
	return mux
}

func main() {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "config.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err = json.Unmarshal(data, &cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	controller, err := NewController(cfg.Controller, hardwareIO(cfg), time.Now())
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go controller.Run(ctx)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: ":" + port, Handler: newMux(controller, func(ctx context.Context) (string, error) { return runSSHCommand(ctx, cfg, "sensors") }), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		server.Shutdown(stop)
	}()
	log.Printf("Starting HTTP server on port %s (mode=%s dry_run=%t)", port, controller.Status().Mode, cfg.Controller.DryRun)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
