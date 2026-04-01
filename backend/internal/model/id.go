package model

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

func NewID(prefix string) string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strings.Trim(prefix, "-")
	}
	if prefix == "" {
		return hex.EncodeToString(raw[:])
	}
	return strings.Trim(prefix, "-") + "-" + hex.EncodeToString(raw[:])
}
