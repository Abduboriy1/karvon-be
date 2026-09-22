// Command secret prints a fresh 32-byte KARVON_SECRET_KEY as hex. The value encrypts
// provider API keys at rest, so it belongs in the environment, never in the repo.
package main

import (
	"fmt"
	"os"

	"github.com/bory/karvon-be/internal/crypto"
)

func main() {
	key, err := crypto.GenerateKeyHex()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(key)
}
