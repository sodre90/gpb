package main

import (
	"fmt"
	"strings"
	"testing"
)

const chromeCopyAsCurl = `curl 'https://photos.google.com/_/PhotosUi/data/batchexecute?rpcids=Fake1d&source-path=%2F&f.sid=-987654321&bl=boq_photosui_20260801.00_p0&hl=en&_reqid=123456&rt=c' \
  -H 'accept: */*' \
  -H 'accept-language: en-US,en;q=0.9' \
  -H 'content-type: application/x-www-form-urlencoded;charset=UTF-8' \
  -H 'user-agent: Mozilla/5.0 (X11; Linux x86_64) Chrome/140.0.0.0 Safari/537.36' \
  -b 'SID=aaa; HSID=bbb; SSID=ccc; APISID=ddd; SAPISID=eee; __Secure-1PSID=fff' \
  --data-raw 'f.req=%5B%5B%5B%22Fake1d%22%2C%22%5Bnull%2Cnull%2C2%5D%22%2Cnull%2C%22generic%22%5D%5D%5D&at=tok_ABC123%3A1700000000%3A&' \
  --compressed`

func TestParseCurlExtractsRequest(t *testing.T) {
	capture, err := ParseCurl(chromeCopyAsCurl)
	if err != nil {
		t.Fatalf("ParseCurl: %v", err)
	}

	if capture.Method != "POST" {
		t.Errorf("Method = %q, want POST", capture.Method)
	}
	if !strings.HasPrefix(capture.URL, "https://photos.google.com/_/PhotosUi/data/batchexecute?") {
		t.Errorf("URL = %q", capture.URL)
	}
	if got := capture.Headers.Get("User-Agent"); !strings.Contains(got, "Chrome/140") {
		t.Errorf("User-Agent = %q", got)
	}
	if got, want := len(capture.CookieNames()), 6; got != want {
		t.Errorf("cookie count = %d, want %d (%v)", got, want, capture.CookieNames())
	}
}

func TestParseCurlDecodesBatchExecuteBody(t *testing.T) {
	capture, err := ParseCurl(chromeCopyAsCurl)
	if err != nil {
		t.Fatalf("ParseCurl: %v", err)
	}

	calls, token, err := DecodeRequestBody(capture.Body)
	if err != nil {
		t.Fatalf("DecodeRequestBody: %v", err)
	}
	if token != "tok_ABC123:1700000000:" {
		t.Errorf("token = %q", token)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].RPCID != "Fake1d" {
		t.Errorf("RPCID = %q", calls[0].RPCID)
	}
	if calls[0].Payload != "[null,null,2]" {
		t.Errorf("Payload = %q", calls[0].Payload)
	}
}

func TestParseCurlHandlesAnsiCQuotingAndCookieHeader(t *testing.T) {
	capture, err := ParseCurl(`curl 'https://example.test/x' ` +
		`-H $'cookie: SID=a\x3db; NID=don\'t' ` +
		`--data-raw $'f.req=%5B%5D&at=x\n'`)
	if err != nil {
		t.Fatalf("ParseCurl: %v", err)
	}
	if got := capture.Headers.Get("Cookie"); got != "SID=a=b; NID=don't" {
		t.Errorf("Cookie = %q", got)
	}
	if !strings.HasSuffix(capture.Body, "\n") {
		t.Errorf("Body lost its ANSI-C newline: %q", capture.Body)
	}
}

func TestRequestBodyRoundTrip(t *testing.T) {
	calls := []Call{{RPCID: "Fake1d", Payload: `["album",null,50]`}}

	decoded, token, err := DecodeRequestBody(EncodeRequestBody(calls, "tok"))
	if err != nil {
		t.Fatalf("DecodeRequestBody: %v", err)
	}
	if token != "tok" {
		t.Errorf("token = %q", token)
	}
	if len(decoded) != 1 || decoded[0] != calls[0] {
		t.Errorf("round trip = %+v, want %+v", decoded, calls)
	}
}

func TestDecodeResponseSplitsChunkedFrames(t *testing.T) {
	raw := ")]}'\n\n" +
		chunk(`[["wrb.fr","Fake1d","[[\"id-1\",\"Holidays\"]]",null,null,null,"generic"]]`) +
		chunk(`[["di",44],["af.httprm",44,"7391",5]]`)

	frames, err := DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %+v", len(frames), frames)
	}
	if frames[0].Tag != "wrb.fr" || frames[0].RPCID != "Fake1d" {
		t.Errorf("frame 0 = %+v", frames[0])
	}
	if frames[0].Payload != `[["id-1","Holidays"]]` {
		t.Errorf("payload = %q", frames[0].Payload)
	}
	if frames[1].Tag != "di" {
		t.Errorf("frame 1 = %+v", frames[1])
	}
}

func TestDecodeResponseHandlesNonASCIIChunkLengths(t *testing.T) {
	payload := `[["wrb.fr","F2A0H","[[[1,\"Családi 2026\",null,1150]]]",null,null,null,"generic"]]`
	raw := ")]}'\n\n" + fmt.Sprintf("%d\n%s\n", len([]rune(payload))+1, payload)

	frames, err := DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if !strings.Contains(frames[0].Payload, "Családi 2026") {
		t.Errorf("payload lost its non-ASCII title: %q", frames[0].Payload)
	}
}

func TestDecodeResponseRejectsLoginPage(t *testing.T) {
	if _, err := DecodeResponse("<!DOCTYPE html><html><title>Sign in</title>"); err == nil {
		t.Fatal("want an error for an HTML login page, got nil")
	}
}

func TestRetargetURLSwapsRPCID(t *testing.T) {
	target, err := RetargetURL("https://photos.google.com/_/PhotosUi/data/batchexecute?rpcids=Old&bl=keep&_reqid=1", "New", 4242)
	if err != nil {
		t.Fatalf("RetargetURL: %v", err)
	}
	for _, want := range []string{"rpcids=New", "bl=keep", "_reqid=4242", "rt=c"} {
		if !strings.Contains(target, want) {
			t.Errorf("target %q missing %q", target, want)
		}
	}
}

func chunk(payload string) string {
	return fmt.Sprintf("%d\n%s\n", len(payload)+1, payload)
}
