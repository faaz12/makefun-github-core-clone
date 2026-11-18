// main.go — entry lifecycle: bind an ephemeral port, register it with the
// env, wait until the env reports the trial is over. Port of main.ts.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const fetchTimeout = 30 * time.Second
const retryBaseMs = 250
const retryCapMs = 2000

func backoffMs(attempt int) time.Duration {
	ms := retryBaseMs
	for i := 0; i < attempt && i < 8; i++ {
		ms *= 2
	}
	if ms > retryCapMs {
		ms = retryCapMs
	}
	return time.Duration(ms) * time.Millisecond
}

// minimal HTTP client over raw TCP (no net/http: keeps the binary small).
// Returns the status code and body.
func httpCall(method, rawURL string, body []byte, contentType string) (int, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, fetchTimeout)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(fetchTimeout))
	var req strings.Builder
	req.WriteString(method + " " + u.RequestURI() + " HTTP/1.1\r\n")
	req.WriteString("Host: " + u.Host + "\r\n")
	if contentType != "" {
		req.WriteString("Content-Type: " + contentType + "\r\n")
	}
	req.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	req.WriteString("Connection: close\r\n\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		return 0, nil, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			return 0, nil, err
		}
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	parts := strings.SplitN(strings.TrimRight(statusLine, "\r\n"), " ", 3)
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("bad status line %q", statusLine)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, err
	}
	contentLength := -1
	chunked := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if i := strings.IndexByte(line, ':'); i >= 0 {
			k := strings.TrimSpace(line[:i])
			v := strings.TrimSpace(line[i+1:])
			if strings.EqualFold(k, "Content-Length") {
				contentLength, _ = strconv.Atoi(v)
			}
			if strings.EqualFold(k, "Transfer-Encoding") && strings.Contains(strings.ToLower(v), "chunked") {
				chunked = true
			}
		}
	}
	var respBody []byte
	if chunked {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return 0, nil, err
			}
			line = strings.TrimRight(line, "\r\n")
			if i := strings.IndexByte(line, ';'); i >= 0 {
				line = line[:i]
			}
			size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
			if err != nil {
				return 0, nil, err
			}
			if size == 0 {
				break
			}
			chunk := make([]byte, size)
			if _, err := io.ReadFull(br, chunk); err != nil {
				return 0, nil, err
			}
			respBody = append(respBody, chunk...)
			if _, err := br.ReadString('\n'); err != nil {
				return 0, nil, err
			}
		}
	} else if contentLength >= 0 {
		respBody = make([]byte, contentLength)
		if _, err := io.ReadFull(br, respBody); err != nil {
			return 0, nil, err
		}
	} else {
		respBody, err = io.ReadAll(br)
		if err != nil {
			return 0, nil, err
		}
	}
	return status, respBody, nil
}

func registerPort(envURL string, port int) error {
	body := []byte(`{"port":` + strconv.Itoa(port) + `}`)
	for attempt := 0; ; attempt++ {
		status, _, err := httpCall("POST", envURL+"/register", body, "application/json")
		if err == nil {
			if status == 200 || status == 409 {
				return nil
			}
			if status == 400 || status == 405 {
				return fmt.Errorf("register answered HTTP %d", status)
			}
		}
		time.Sleep(backoffMs(attempt))
	}
}

func waitUntilDone(envURL string) error {
	for attempt := 0; ; attempt++ {
		status, body, err := httpCall("GET", envURL+"/done", nil, "")
		if err == nil && status == 200 {
			if strings.Contains(string(body), `"done":true`) || strings.Contains(string(body), `"done": true`) {
				return nil
			}
		}
		time.Sleep(backoffMs(attempt))
	}
}

func main() {
	initTrialNow()
	initToolsList()
	store = NewStore()

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}

	l, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	addr := l.Addr().(*net.TCPAddr)
	fmt.Fprintf(os.Stderr, "[entry] listening on port %d\n", addr.Port)
	go serve(l)

	if err := registerPort(envURL, addr.Port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "[entry] registered; waiting for the env to say done")

	if err := waitUntilDone(envURL); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	_ = l.Close()
	fmt.Fprintln(os.Stderr, "[entry] done")
}
