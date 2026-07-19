// Package auth implements databox's identity model: users, password
// hashing, session tokens, S3 access keys, and the prefix-based grant
// system (§7).
//
// This file covers password hashing. The requirements are strict:
//
//   - Passwords are hashed with argon2id (the current OWASP-recommended
//     memory-hard algorithm, resistant to GPU cracking).
//   - The derived key is 64 bytes = 512 bits. The project mandates that no
//     password hash is ever shorter than 512 bits.
//   - Every user gets a fresh 32-byte random salt, so identical passwords
//     never produce identical hashes.
//
// The encoded hash format is self-describing, similar to the standard
// $argon2id$ notation, so parameters can be strengthened in the future
// without invalidating existing hashes:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<base64 salt>$<base64 512-bit key>
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2 parameters. These follow the RFC 9106 "memory-constrained"
// recommendation upgraded for server hardware: 64 MiB memory, 3 passes,
// 2 lanes. Raising them later is safe — old hashes carry their own params.
const (
	argonMemoryKiB = 64 * 1024 // 64 MiB of memory per hash
	argonPasses    = 3         // time cost: number of passes over memory
	argonLanes     = 2         // parallelism: independent memory lanes
	saltBytes      = 32        // 256-bit random salt per user
	keyBytes       = 64        // 512-bit derived key — the project minimum
)

// HashPassword derives a 512-bit argon2id hash for the given password with
// a freshly generated random salt, returning the self-describing encoded
// form that VerifyPassword understands.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		// rand.Read failing means the OS entropy source is broken;
		// nothing sensible can be done except refuse to create the hash.
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonPasses, argonMemoryKiB, argonLanes, keyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonPasses, argonLanes,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a candidate password against a stored encoded hash.
// The comparison is constant-time so response timing does not leak how many
// bytes of the hash matched.
func VerifyPassword(password, encoded string) bool {
	// Split "$argon2id$v=19$m=...,t=...,p=...$salt$key" into its parts.
	// The leading '$' produces an empty first element.
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var mem, passes uint32
	var lanes uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &passes, &lanes); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	// Recompute with the parameters stored in the hash itself, then
	// compare in constant time.
	got := argon2.IDKey([]byte(password), salt, passes, mem, lanes, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// RandomToken returns n cryptographically random bytes encoded as URL-safe
// base64. Used for session tokens, S3 secrets, join secrets, and generated
// root passwords.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Same rationale as HashPassword: a dead entropy source is fatal.
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
