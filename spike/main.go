package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command, args := os.Args[1], os.Args[2:]

	var err error
	switch command {
	case "inspect":
		err = runInspect(args)
	case "replay":
		err = runReplay(args)
	case "call":
		err = runCall(args)
	case "get":
		err = runGet(args)
	case "hash":
		err = runHash(args)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `phase-0 spike — probes what Google Photos' web surface allows outside a browser

  inspect <capture.curl>
        Decode a captured request offline. Sends nothing. Reveals rpcids and payload shapes.

  replay <capture.curl>
        Re-issue the captured request verbatim from Go. The core question: does the
        session work outside the browser at all?

  call <capture.curl> <rpcid> <payload-json>
        Reuse the captured session to issue a different RPC.

  get <capture.curl> [url] [-o file] [-anon]
        Authenticated GET, for original-download and thumbnail probes. Defaults to the
        captured request's own URL. -anon drops the cookies, to test whether the URL
        needs a session at all.

  hash <file>
        sha256 of a file, for byte-comparing against the browser's download.
`)
}

func loadCapture(path string) (*CapturedRequest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCurl(string(raw))
}

func runInspect(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: inspect <capture.curl>")
	}
	capture, err := loadCapture(args[0])
	if err != nil {
		return err
	}

	parsed, err := url.Parse(capture.URL)
	if err != nil {
		return err
	}

	fmt.Printf("%s %s\n", capture.Method, parsed.Scheme+"://"+parsed.Host+parsed.Path)
	fmt.Println("\nquery parameters:")
	for key, values := range parsed.Query() {
		fmt.Printf("  %-16s %s\n", key, strings.Join(values, ", "))
	}

	names := capture.CookieNames()
	fmt.Printf("\ncookies: %d present\n  %s\n", len(names), strings.Join(names, " "))

	if agent := capture.Headers.Get("User-Agent"); agent != "" {
		fmt.Printf("\nuser-agent:\n  %s\n", agent)
	}

	calls, token, err := DecodeRequestBody(capture.Body)
	if err != nil {
		fmt.Printf("\nbody is not a batchexecute payload (%v)\n", err)
		return nil
	}

	if token == "" {
		fmt.Println("\nat token: ABSENT — this request carried no CSRF token")
	} else {
		fmt.Printf("\nat token: present (%d chars, starts %.8s...)\n", len(token), token)
	}

	fmt.Printf("\n%d call(s) in this request:\n", len(calls))
	for _, call := range calls {
		fmt.Printf("\n  rpcid %s\n", call.RPCID)
		fmt.Printf("  payload: %s\n", indent(PrettyJSON(call.Payload), "    "))
	}
	return nil
}

func runReplay(args []string) error {
	flags := flag.NewFlagSet("replay", flag.ExitOnError)
	saveDir := flags.String("save", "", "write each wrb.fr payload to this directory as <rpcid>.json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: replay <capture.curl> [-save dir]")
	}
	capture, err := loadCapture(flags.Arg(0))
	if err != nil {
		return err
	}
	return sendBatchExecute(capture, capture.URL, capture.Body, *saveDir)
}

func runCall(args []string) error {
	flags := flag.NewFlagSet("call", flag.ExitOnError)
	saveDir := flags.String("save", "", "write each wrb.fr payload to this directory as <rpcid>.json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 3 {
		return fmt.Errorf("usage: call [-save dir] <capture.curl> <rpcid> <payload-json>")
	}
	capture, err := loadCapture(flags.Arg(0))
	if err != nil {
		return err
	}

	_, token, err := DecodeRequestBody(capture.Body)
	if err != nil {
		return fmt.Errorf("cannot reuse this capture: %w", err)
	}

	rpcID, payload := flags.Arg(1), flags.Arg(2)
	target, err := RetargetURL(capture.URL, rpcID, 100000)
	if err != nil {
		return err
	}
	return sendBatchExecute(capture, target, EncodeRequestBody([]Call{{RPCID: rpcID, Payload: payload}}, token), *saveDir)
}

func sendBatchExecute(capture *CapturedRequest, target, body, saveDir string) error {
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		return err
	}
	for name, values := range capture.Headers {
		if strings.EqualFold(name, "Content-Length") {
			continue
		}
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}

	started := time.Now()
	response, err := newClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}

	fmt.Printf("HTTP %s in %s, %d bytes\n", response.Status, time.Since(started).Round(time.Millisecond), len(raw))
	if warnAboutLoginRedirect(response, string(raw)) {
		return nil
	}

	frames, err := DecodeResponse(string(raw))
	if err != nil {
		fmt.Printf("\ndecode failed: %v\n\nfirst 400 bytes:\n%.400s\n", err, raw)
		return nil
	}

	fmt.Printf("\n%d frame(s):\n", len(frames))
	for _, frame := range frames {
		if frame.Tag != "wrb.fr" {
			fmt.Printf("\n  [%s] %s\n", frame.Tag, frame.RPCID)
			continue
		}
		fmt.Printf("\n  [wrb.fr] rpcid %s, payload %d bytes\n", frame.RPCID, len(frame.Payload))
		if saveDir != "" {
			path := filepath.Join(saveDir, frame.RPCID+".json")
			if err := os.WriteFile(path, []byte(PrettyJSON(frame.Payload)), 0o600); err != nil {
				return err
			}
			fmt.Printf("  saved to %s\n", path)
			continue
		}
		fmt.Println(indent(PrettyJSON(frame.Payload), "    "))
	}
	return nil
}

func runGet(args []string) error {
	flags := flag.NewFlagSet("get", flag.ExitOnError)
	output := flags.String("o", "", "write the body to this file")
	anonymous := flags.Bool("anon", false, "send no cookies")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() < 1 || flags.NArg() > 2 {
		return fmt.Errorf("usage: get <capture.curl> [url] [-o file] [-anon]")
	}

	capture, err := loadCapture(flags.Arg(0))
	if err != nil {
		return err
	}

	target := capture.URL
	if flags.NArg() == 2 {
		target = flags.Arg(1)
	}

	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "*/*")
	if agent := capture.Headers.Get("User-Agent"); agent != "" {
		request.Header.Set("User-Agent", agent)
	}
	if language := capture.Headers.Get("Accept-Language"); language != "" {
		request.Header.Set("Accept-Language", language)
	}
	var redirects []string
	client := newClient()
	if !*anonymous {
		jar, err := jarFromCapture(capture)
		if err != nil {
			return err
		}
		client.Jar = jar
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		redirects = append(redirects, req.URL.String())
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}

	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	fmt.Printf("cookies sent: %t\n", !*anonymous)
	fmt.Printf("HTTP %s in %s\n", response.Status, time.Since(started).Round(time.Millisecond))
	for _, header := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Cache-Control", "Expires"} {
		if value := response.Header.Get(header); value != "" {
			fmt.Printf("  %-20s %s\n", header, value)
		}
	}
	if len(redirects) > 0 {
		fmt.Println("\nredirect chain:")
		for _, location := range redirects {
			fmt.Printf("  -> %.160s\n", location)
		}
	}

	digest := sha256.New()
	var sink io.Writer = digest
	if *output != "" {
		file, err := os.Create(*output)
		if err != nil {
			return err
		}
		defer file.Close()
		sink = io.MultiWriter(digest, file)
	}

	written, err := io.Copy(sink, response.Body)
	if err != nil {
		return err
	}

	fmt.Printf("\nbody: %d bytes, sha256 %s\n", written, hex.EncodeToString(digest.Sum(nil)))
	if *output != "" {
		fmt.Printf("saved to %s\n", *output)
	}
	if response.StatusCode != http.StatusOK {
		fmt.Println("\nnon-200 — this URL did not serve the file under these conditions")
	}
	return nil
}

func runHash(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: hash <file>")
	}
	file, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer file.Close()

	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return err
	}
	fmt.Printf("%s  %d bytes  %s\n", hex.EncodeToString(digest.Sum(nil)), size, args[0])
	return nil
}

func newClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

func jarFromCapture(capture *CapturedRequest) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	var cookies []*http.Cookie
	for _, pair := range strings.Split(capture.Cookie(), ";") {
		name, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || name == "" {
			continue
		}
		cookies = append(cookies, &http.Cookie{
			Name:   name,
			Value:  value,
			Domain: "google.com",
			Path:   "/",
			Secure: true,
		})
	}

	jar.SetCookies(&url.URL{Scheme: "https", Host: "www.google.com"}, cookies)
	return jar, nil
}

func warnAboutLoginRedirect(response *http.Response, body string) bool {
	if strings.Contains(response.Request.URL.Host, "accounts.google.com") ||
		strings.Contains(body, "signin/rejected") {
		fmt.Println("\nthe session was rejected — you were bounced to a login page.")
		fmt.Println("re-copy the request from a freshly loaded, logged-in tab.")
		return true
	}
	return false
}

func indent(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if i > 0 {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
