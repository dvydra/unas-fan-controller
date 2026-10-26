package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPManualAndMode(t *testing.T) {
	f := &fakeHardware{pwm: [2]int{38, 38}, disk: 33, ambient: 22}
	c, _ := testController(t, f, time.Now(), false)
	h := newMux(c, func(context.Context) (string, error) { return "fixture sensors", nil })
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	for _, body := range []string{`{}`, `{"speed":null}`, `{"speed":1.5}`, `{"speed":-1}`, `{"speed":256}`, `{"speed":40} {}`, strings.Repeat("x", 5000)} {
		w := request("POST", "/api/fan", body)
		if w.Code != 400 {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if f.writes != 0 {
		t.Fatal("invalid payload actuated fan")
	}
	if w := request("POST", "/api/fan", `{"speed":60}`); w.Code != 200 || f.pwm != [2]int{60, 60} || c.Status().Mode != "manual" {
		t.Fatal(w.Code, w.Body.String(), f.pwm)
	}
	if w := request("POST", "/api/mode", `{"mode":"auto"}`); w.Code != 200 || c.Status().Mode != "auto" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("GET", "/api/fan", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"speed":60`) {
		t.Fatal(w.Body.String())
	}
	if w := request("GET", "/api/controller", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"auto"`) {
		t.Fatal(w.Body.String())
	}
	if w := request("PUT", "/api/mode", `{}`); w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code)
	}
}
