package gphotos

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// responseGuard prefixes every batchexecute response. It exists to make the body invalid
// JavaScript, defeating cross-origin <script> inclusion; we simply strip it.
const responseGuard = ")]}'"

// frame is one RPC answer inside a batched response. A single request may be answered by
// several frames, only some of which carry the rpcid we asked about.
type frame struct {
	tag     string
	rpcID   string
	payload string
}

func encodeRequest(rpcID string, payload []any) (string, error) {
	inner, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding the %s payload: %w", rpcID, err)
	}

	envelope, err := json.Marshal([]any{[]any{[]any{rpcID, string(inner), nil, "generic"}}})
	if err != nil {
		return "", fmt.Errorf("encoding the %s envelope: %w", rpcID, err)
	}
	return string(envelope), nil
}

// decodeFrames walks the chunked response body. Chunks are length-prefixed with a decimal
// line, but the length counts UTF-16 code units rather than bytes, so it cannot be used to
// slice a Go string; the decoder streams instead and trusts JSON's own framing.
func decodeFrames(body string) ([]frame, error) {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, responseGuard) {
		if servedAPage(trimmed) {
			return nil, fmt.Errorf("%w: batchexecute answered with an HTML page", ErrSessionRejected)
		}
		return nil, fmt.Errorf("%w: response lacks the %q guard prefix", ErrProtocolDrift, responseGuard)
	}
	trimmed = strings.TrimPrefix(trimmed, responseGuard)

	var frames []frame
	for cursor := 0; cursor < len(trimmed); {
		cursor += skipChunkHeader(trimmed[cursor:])
		if cursor >= len(trimmed) {
			break
		}

		decoder := json.NewDecoder(strings.NewReader(trimmed[cursor:]))
		var rows [][]json.RawMessage
		if err := decoder.Decode(&rows); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return frames, fmt.Errorf("%w: chunk at offset %d is not an array of frames: %v", ErrProtocolDrift, cursor, err)
		}
		cursor += int(decoder.InputOffset())
		frames = append(frames, framesFromRows(rows)...)
	}
	return frames, nil
}

// servedAPage reports whether an rpc was answered with a document. Google serves the signed-out
// shell with a 200 and no redirect to follow, so a session that died between one request and the
// next arrives here as HTML — and read as drift it stops the run with "Google changed something",
// which sends the user hunting for a bug instead of to the sign-in button.
func servedAPage(body string) bool {
	opening := strings.ToLower(body)
	return strings.HasPrefix(opening, "<!doctype html") || strings.HasPrefix(opening, "<html")
}

func skipChunkHeader(text string) int {
	cursor := 0
	for cursor < len(text) && unicode.IsSpace(rune(text[cursor])) {
		cursor++
	}

	digitsEnd := cursor
	for digitsEnd < len(text) && text[digitsEnd] >= '0' && text[digitsEnd] <= '9' {
		digitsEnd++
	}
	if digitsEnd == cursor || digitsEnd >= len(text) || text[digitsEnd] != '\n' {
		return cursor
	}

	for digitsEnd < len(text) && unicode.IsSpace(rune(text[digitsEnd])) {
		digitsEnd++
	}
	return digitsEnd
}

func framesFromRows(rows [][]json.RawMessage) []frame {
	var decoded []frame
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		var next frame
		if err := json.Unmarshal(row[0], &next.tag); err != nil {
			continue
		}
		next.rpcID = optionalString(row, 1)
		next.payload = optionalString(row, 2)
		decoded = append(decoded, next)
	}
	return decoded
}

// optionalString leaves a field empty rather than failing when it is not a string. Google
// appends bookkeeping frames ("di", "af.httprm") that carry numbers where an answer carries
// strings, and a missing answer is still caught by payloadFor, which names what did arrive.
func optionalString(row []json.RawMessage, index int) string {
	if index >= len(row) {
		return ""
	}
	var text string
	if err := json.Unmarshal(row[index], &text); err != nil {
		return ""
	}
	return text
}

// payloadFor picks the frame answering one rpcid and decodes its doubly-encoded payload.
// Google wraps the real answer in a JSON string inside the frame array.
func payloadFor(frames []frame, rpcID string) (any, error) {
	for _, candidate := range frames {
		if candidate.rpcID != rpcID || candidate.payload == "" {
			continue
		}
		var decoded any
		if err := json.Unmarshal([]byte(candidate.payload), &decoded); err != nil {
			return nil, fmt.Errorf("%w: the %s payload is not JSON: %v", ErrProtocolDrift, rpcID, err)
		}
		return decoded, nil
	}
	return nil, fmt.Errorf("%w: no %s frame in a response carrying %s", ErrProtocolDrift, rpcID, describeFrames(frames))
}

func describeFrames(frames []frame) string {
	if len(frames) == 0 {
		return "no frames"
	}
	ids := make([]string, 0, len(frames))
	for _, one := range frames {
		if one.rpcID != "" {
			ids = append(ids, one.rpcID)
		}
	}
	if len(ids) == 0 {
		return fmt.Sprintf("%d unnamed frames", len(frames))
	}
	return strings.Join(ids, ", ")
}
