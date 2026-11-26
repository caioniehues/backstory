package embed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// TemplateVersion names the prompt template, DocumentPrefix and QueryPrefix
// together: it is derived from both, so changing either bumps it and every
// Record is re-embedded.
var TemplateVersion = templateVersion(DocumentPrefix, QueryPrefix)

func templateVersion(doc, query string) string {
	sum := sha256.Sum256([]byte(doc + "\n" + query))
	return "tpl-" + hex.EncodeToString(sum[:4])
}

// Fingerprint is a vector's record_embeddings.model: the model file's
// sha256, TemplateVersion, the backend and the dimension, e.g.
// "2188ac1d…|tpl-ecc4bd80|vulkan|768" (#20 and its amendment).
func Fingerprint(modelSHA256, backend string, dim int) string {
	return modelSHA256 + "|" + TemplateVersion + "|" + backend + "|" + strconv.Itoa(dim)
}

// ModelHash is the sha256 of the model file at path. It is cached in the
// Device-local SettingModelHash, keyed by path, size and mtime, so the
// 310 MB file isn't hashed again at each start.
func ModelHash(s Settings, path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("embed: model %s: %w", path, err)
	}
	key := strconv.FormatInt(fi.Size(), 10) + " " + strconv.FormatInt(fi.ModTime().UnixNano(), 10) + " "
	if v, ok, err := s.GetSetting(SettingModelHash); err != nil {
		return "", err
	} else if ok {
		// "<size> <mtime ns> <sha256> <path>": the path goes last, as it
		// may hold spaces.
		if f := strings.SplitN(v, " ", 4); len(f) == 4 && f[0]+" "+f[1]+" " == key && f[3] == path {
			return f[2], nil
		}
	}
	f, err := os.Open(path) //nolint:gosec // the Human's chosen model file
	if err != nil {
		return "", fmt.Errorf("embed: model %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("embed: hashing %s: %w", path, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := s.SetSetting(SettingModelHash, key+sum+" "+path); err != nil {
		return "", err
	}
	return sum, nil
}
