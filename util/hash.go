package util

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// HashBytes accepts a bytes object and returns a string sha256 hash representing that object.
func HashBytes(b []byte) string {
	hash := sha256.New()
	hash.Write(b)

	return hex.EncodeToString(hash.Sum(nil))
}

// HashObject accepts any object, dumps it to json then sends it to HashBytes.
func HashObject(o any) ([]byte, string, error) { //nolint:gocritic
	b, err := json.Marshal(o)
	if err != nil {
		return nil, "", err
	}

	return b, HashBytes(b), nil
}

// HashObjectYAML accepts any object, dumps it to yaml then sends it to HashBytes.
func HashObjectYAML(o any) ([]byte, string, error) { //nolint:gocritic
	// NOTE: We intentionally marshal to JSON here.
	//
	// YAML serialization of Go maps is not stable (map iteration order), which causes
	// flapping hashes and endless reconcile loops. JSON marshaling is stable (sorted
	// map keys) and the output is valid YAML (YAML is a superset of JSON).
	b, err := json.Marshal(o)
	if err != nil {
		return nil, "", err
	}

	return b, HashBytes(b), nil
}
