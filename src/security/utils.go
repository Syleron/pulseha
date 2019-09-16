package security

import (
	"crypto/sha256"
	"encoding/hex"
)

// GenerateSHA256Hash - Generate a sha256 hash for a particular string
func GenerateSHA256Hash(str string) string {
	h := sha256.New()
	h.Write([]byte(str))
	sha256_hash := hex.EncodeToString(h.Sum(nil))
	return sha256_hash
}

// SHA256StringValidation - Validate a string matches a generated hash
func SHA256StringValidation(str string, strHash string) bool {
	hash := GenerateSHA256Hash(str)
	if strHash  == hash {
		return true
	}
	return false
}
