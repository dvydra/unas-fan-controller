# UNAS Fan & Temperature Controller

A simple web application to safely monitor temperatures and control fan speeds on a UNAS device via SSH.

## Building and Running locally

This application is written in Go and requires no external dependencies (only standard library and `golang.org/x/crypto/ssh`). 

1. Edit the `config.json` file to point to your NAS and provide the necessary SSH credentials (password or path to identity file).
2. Start the application:
   ```bash
   go run main.go
   ```
3. Open your browser to http://localhost:8080

## Configuration

By default, the application looks for `config.json` in its working directory. You can override the path using the `CONFIG_PATH` environment variable:
```bash
CONFIG_PATH=/opt/unas-fan-controller/config.json go run main.go
```

You can optionally change the port using the `PORT` environment variable:
```bash
PORT=3000 go run main.go
```
