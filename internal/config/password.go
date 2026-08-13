package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

var ErrMalformedHash = errors.New("password hash is not a valid argon2id PHC string")

func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return encodePHC(salt, key), nil
}

func VerifyPassword(encoded, password string) (bool, error) {
	salt, want, params, err := decodePHC(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func encodePHC(salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

func decodePHC(encoded string) (salt, key []byte, params argonParams, err error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" || fields[1] != "argon2id" {
		return nil, nil, params, ErrMalformedHash
	}

	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil {
		return nil, nil, params, ErrMalformedHash
	}
	if version != argon2.Version {
		return nil, nil, params, fmt.Errorf("%w: unsupported version %d", ErrMalformedHash, version)
	}
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &params.memory, &params.time, &params.threads); err != nil {
		return nil, nil, params, ErrMalformedHash
	}

	if salt, err = base64.RawStdEncoding.DecodeString(fields[4]); err != nil {
		return nil, nil, params, ErrMalformedHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(fields[5]); err != nil {
		return nil, nil, params, ErrMalformedHash
	}
	if len(salt) == 0 || len(key) == 0 {
		return nil, nil, params, ErrMalformedHash
	}
	return salt, key, params, nil
}
