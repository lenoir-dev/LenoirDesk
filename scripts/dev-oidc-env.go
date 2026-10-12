package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func main() {
	raw, err := os.ReadFile(".env")
	if err != nil {
		panic(err)
	}
	if strings.Contains(string(raw), "RELAYDESK_OIDC_CLIENT_SECRET=") {
		return
	}
	password := randomHex(16)
	secret := randomHex(24)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	f, err := os.OpenFile(".env", os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		panic(err)
	}
	defer func() { _ = f.Close() }()
	// Single quotes keep bcrypt dollar signs literal in Compose dotenv parsing.
	_, err = fmt.Fprintf(f, "RELAYDESK_OIDC_CLIENT_SECRET=%s\nRELAYDESK_DEV_OIDC_PASSWORD=%s\nRELAYDESK_DEV_OIDC_HASH='%s'\n", secret, password, hash)
	if err != nil {
		panic(err)
	}
}
