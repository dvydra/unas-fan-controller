package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

//go:embed index.html
var indexHTML []byte

type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	KeyFile  string `json:"key_file"`
}

var (
	config Config
	sshMu  sync.Mutex
)

// loadConfig reads the configuration file from a specified path
func loadConfig(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return err
	}

	if config.Port == 0 {
		config.Port = 22
	}

	return nil
}

// getSSHClient configures and connects to the SSH server using the loaded config
func getSSHClient() (*ssh.Client, error) {
	var authMethods []ssh.AuthMethod

	if config.Password != "" {
		authMethods = append(authMethods, ssh.Password(config.Password))
	}

	if config.KeyFile != "" {
		key, err := os.ReadFile(config.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read private key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("unable to parse private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods provided in config")
	}

	sshConfig := &ssh.ClientConfig{
		User:            config.User,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // For an internal NAS, insecure host key is often acceptable
		Timeout:         5 * time.Second,
	}

	address := fmt.Sprintf("%s:%d", config.Host, config.Port)
	client, err := ssh.Dial("tcp", address, sshConfig)
	if err != nil {
		return nil, err
	}

	return client, nil
}

// runSSHCommand connects via SSH, runs a single command, and returns its standard output text.
func runSSHCommand(cmd string) (string, error) {
	sshMu.Lock()
	defer sshMu.Unlock()

	client, err := getSSHClient()
	if err != nil {
		return "", fmt.Errorf("failed to connect to SSH: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	var stdoutBuf strings.Builder
	var stderrBuf strings.Builder
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	if err := session.Run(cmd); err != nil {
		return "", fmt.Errorf("failed to run command '%s': %w, stderr: %s", cmd, err, stderrBuf.String())
	}

	return stdoutBuf.String(), nil
}

func handleSensors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	output, err := runSSHCommand("sensors")
	if err != nil {
		log.Printf("Error running sensors: %v\n", err)
		http.Error(w, "Failed to get sensors output", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(output))
}

type FanRequest struct {
	Speed int `json:"speed"`
}

func handleFan(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		output, err := runSSHCommand("cat /sys/class/hwmon/hwmon0/pwm1")
		if err != nil {
			log.Printf("Error getting fan speed: %v\n", err)
			http.Error(w, "Failed to get fan speed", http.StatusInternalServerError)
			return
		}

		speed := strings.TrimSpace(output)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fmt.Sprintf(`{"speed": %s}`, speed)))
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req FanRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Defensive constraint validation
	if req.Speed < 0 || req.Speed > 255 {
		http.Error(w, "Speed must be between 0 and 255", http.StatusBadRequest)
		return
	}

	// Security: using strictly parameterized ints, we execute a fixed command template
	cmd := fmt.Sprintf("echo %d > /sys/class/hwmon/hwmon0/pwm1 && echo %d > /sys/class/hwmon/hwmon0/pwm2", req.Speed, req.Speed)

	_, err = runSSHCommand(cmd)
	if err != nil {
		log.Printf("Error setting fan speed: %v\n", err)
		http.Error(w, "Failed to set fan speed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "success", "message": "Fan speed updated"}`))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write(indexHTML)
}

func main() {
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "config.json"
	}

	if err := loadConfig(configPath); err != nil {
		log.Fatalf("Failed to load configuration from %s: %v", configPath, err)
	}

	log.Printf("Configuration loaded for UNAS at %s:%d (User: %s)", config.Host, config.Port, config.User)

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/sensors", handleSensors)
	http.HandleFunc("/api/fan", handleFan)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting HTTP server on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
