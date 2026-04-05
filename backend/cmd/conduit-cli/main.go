package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "health":
		return runHealth(args[1:], stdout, stderr)
	case "stats":
		return runStats(args[1:], stdout, stderr)
	case "create-key":
		return runCreateKey(args[1:], stdout, stderr)
	case "print-env":
		return runPrintEnv(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runHealth(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseURL := fs.String("base-url", "http://127.0.0.1:8080", "Conduit base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(strings.TrimRight(*baseURL, "/") + "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "health request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		fmt.Fprintf(stderr, "read health response failed: %v\n", err)
		return 1
	}
	return 0
}

func runStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseURL := fs.String("base-url", "http://127.0.0.1:8080", "Conduit base URL")
	adminToken := fs.String("admin-token", "", "Admin token")
	window := fs.String("window", "7d", "Stats window (today, 7d, 30d)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*adminToken) == "" {
		fmt.Fprintln(stderr, "--admin-token is required")
		return 2
	}

	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(*baseURL, "/")+"/api/admin/stats/summary?window="+*window, nil)
	if err != nil {
		fmt.Fprintf(stderr, "build stats request failed: %v\n", err)
		return 1
	}
	req.Header.Set("X-Admin-Token", strings.TrimSpace(*adminToken))
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "stats request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(stderr, "stats request failed with status %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		fmt.Fprintf(stderr, "read stats response failed: %v\n", err)
		return 1
	}
	return 0
}

func runCreateKey(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("create-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseURL := fs.String("base-url", "http://127.0.0.1:8080", "Conduit base URL")
	adminToken := fs.String("admin-token", "", "Admin token")
	name := fs.String("name", "", "Gateway key name")
	notes := fs.String("notes", "", "Gateway key notes")
	models := fs.String("models", "", "Comma-separated allowed model aliases")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*adminToken) == "" || strings.TrimSpace(*name) == "" {
		fmt.Fprintln(stderr, "--admin-token and --name are required")
		return 2
	}

	payload := map[string]any{
		"name":  strings.TrimSpace(*name),
		"notes": strings.TrimSpace(*notes),
	}
	if strings.TrimSpace(*models) != "" {
		items := strings.Split(*models, ",")
		allowedModels := make([]string, 0, len(items))
		for _, item := range items {
			if value := strings.TrimSpace(item); value != "" {
				allowedModels = append(allowedModels, value)
			}
		}
		payload["allowed_models"] = allowedModels
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(stderr, "encode create-key payload failed: %v\n", err)
		return 1
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(*baseURL, "/")+"/api/admin/gateway-keys", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "build create-key request failed: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", strings.TrimSpace(*adminToken))
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "create-key request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(stderr, "create-key request failed with status %d: %s\n", resp.StatusCode, strings.TrimSpace(string(responseBody)))
		return 1
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		fmt.Fprintf(stderr, "read create-key response failed: %v\n", err)
		return 1
	}
	return 0
}

func runPrintEnv(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("print-env", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseURL := fs.String("base-url", "http://127.0.0.1:8080", "Conduit base URL")
	apiKey := fs.String("api-key", "", "Gateway API key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*apiKey) == "" {
		fmt.Fprintln(stderr, "--api-key is required")
		return 2
	}

	base := strings.TrimRight(*baseURL, "/")
	fmt.Fprintf(stdout, "export OPENAI_BASE_URL=%q\n", base+"/v1")
	fmt.Fprintf(stdout, "export OPENAI_API_KEY=%q\n", strings.TrimSpace(*apiKey))
	fmt.Fprintf(stdout, "export ANTHROPIC_BASE_URL=%q\n", base)
	fmt.Fprintf(stdout, "export ANTHROPIC_API_KEY=%q\n", strings.TrimSpace(*apiKey))
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: conduit-cli <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  health      Fetch /healthz")
	fmt.Fprintln(w, "  stats       Fetch /api/admin/stats/summary")
	fmt.Fprintln(w, "  create-key  Create a gateway key via the admin API")
	fmt.Fprintln(w, "  print-env   Print shell exports for OpenAI/Anthropic-compatible clients")
}
