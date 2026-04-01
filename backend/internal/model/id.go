package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

func NewID(prefix string) string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Errorf("model.NewID: crypto/rand failed: %w", err))
	}
	if prefix == "" {
		return hex.EncodeToString(raw[:])
	}
	return strings.Trim(prefix, "-") + "-" + hex.EncodeToString(raw[:])
}
