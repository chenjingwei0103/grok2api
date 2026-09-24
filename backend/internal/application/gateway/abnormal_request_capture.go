package gateway

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type abnormalRequestFileCaptureConfig struct {
	Enabled   bool
	Directory string
}

func writeAbnormalRequestFile(config abnormalRequestFileCaptureConfig, body []byte) (string, error) {
	if !config.Enabled || len(body) == 0 {
		return "", nil
	}
	directory := strings.TrimSpace(config.Directory)
	if directory == "" {
		return "", fmt.Errorf("abnormal request capture directory is required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create abnormal request capture directory: %w", err)
	}
	digest := sha256.Sum256(body)
	name := fmt.Sprintf("quality-withhold-%s-%x.json", time.Now().UTC().Format("20060102T150405.000000000Z"), digest[:8])
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create abnormal request capture: %w", err)
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write abnormal request capture: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close abnormal request capture: %w", err)
	}
	return path, nil
}
