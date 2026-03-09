// Package updater 提供從 GitHub Releases 自動更新 radb 執行檔的功能。
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/chris1004tw/remote-adb/internal/buildinfo"
)

// Updater 負責檢查與執行 binary 更新。
type Updater struct {
	Source Source
}

// NewUpdater 建立使用 GitHub Releases 的 Updater。
func NewUpdater() *Updater {
	return &Updater{Source: NewGitHubSource()}
}

// CheckResult 包含版本檢查的結果。
type CheckResult struct {
	CurrentVersion string
	LatestVersion  string
	HasUpdate      bool
	AssetName      string
}

// Check 檢查是否有新版本可用。
func (u *Updater) Check(ctx context.Context) (*CheckResult, error) {
	info, err := u.Source.LatestRelease(ctx)
	if err != nil {
		return nil, err
	}

	current := buildinfo.Version
	result := &CheckResult{
		CurrentVersion: current,
		LatestVersion:  info.TagName,
		AssetName:      info.AssetName,
	}

	// dev 版本永遠視為有更新（除非 remote 也是 dev）
	if current == "dev" {
		result.HasUpdate = info.TagName != "dev"
		return result, nil
	}

	result.HasUpdate = CompareVersions(current, info.TagName) < 0
	return result, nil
}

// Update 下載並安裝最新版本。
func (u *Updater) Update(ctx context.Context) (*CheckResult, error) {
	info, err := u.Source.LatestRelease(ctx)
	if err != nil {
		return nil, fmt.Errorf("取得最新版本失敗: %w", err)
	}

	current := buildinfo.Version
	result := &CheckResult{
		CurrentVersion: current,
		LatestVersion:  info.TagName,
		AssetName:      info.AssetName,
	}

	if current != "dev" && CompareVersions(current, info.TagName) >= 0 {
		result.HasUpdate = false
		return result, nil
	}
	result.HasUpdate = true

	// 建立暫存目錄
	tmpDir, err := os.MkdirTemp("", "radb-update-*")
	if err != nil {
		return nil, fmt.Errorf("建立暫存目錄失敗: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// 下載 archive
	archivePath := filepath.Join(tmpDir, info.AssetName)
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		return nil, fmt.Errorf("建立暫存檔案失敗: %w", err)
	}

	// 同時計算 SHA256
	hash := sha256.New()
	writer := io.MultiWriter(archiveFile, hash)

	if err := u.Source.DownloadAsset(ctx, info.AssetURL, writer); err != nil {
		archiveFile.Close()
		return nil, fmt.Errorf("下載失敗: %w", err)
	}
	archiveFile.Close()

	// 驗證 checksum（如果有 checksums.txt）
	if info.ChecksumURL != "" {
		if err := u.verifyChecksum(ctx, info, hex.EncodeToString(hash.Sum(nil))); err != nil {
			return nil, err
		}
	}

	// 解壓
	extractDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return nil, fmt.Errorf("建立解壓目錄失敗: %w", err)
	}

	extracted, err := ExtractArchive(archivePath, extractDir)
	if err != nil {
		return nil, fmt.Errorf("解壓失敗: %w", err)
	}
	if len(extracted) == 0 {
		return nil, fmt.Errorf("archive 中沒有找到任何 radb binary")
	}

	// 找出目前執行檔所在目錄
	selfPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("取得執行檔路徑失敗: %w", err)
	}
	selfPath, err = filepath.EvalSymlinks(selfPath)
	if err != nil {
		return nil, fmt.Errorf("解析執行檔路徑失敗: %w", err)
	}
	installDir := filepath.Dir(selfPath)

	// 逐一替換
	for _, newBin := range extracted {
		name := filepath.Base(newBin)
		targetPath := filepath.Join(installDir, name)

		// 檢查目標是否存在（只替換已安裝的 binary）
		if _, err := os.Stat(targetPath); os.IsNotExist(err) {
			continue
		}

		if err := ReplaceBinary(targetPath, newBin); err != nil {
			return nil, fmt.Errorf("替換 %s 失敗: %w", name, err)
		}
	}

	return result, nil
}

func (u *Updater) verifyChecksum(ctx context.Context, info *ReleaseInfo, actualHash string) error {
	var buf strings.Builder
	if err := u.Source.DownloadAsset(ctx, info.ChecksumURL, &buf); err != nil {
		return fmt.Errorf("下載 checksums.txt 失敗: %w", err)
	}

	for _, line := range strings.Split(buf.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == info.AssetName {
			if fields[0] != actualHash {
				return fmt.Errorf("checksum 驗證失敗: 預期 %s, 實際 %s", fields[0], actualHash)
			}
			return nil
		}
	}

	// checksums.txt 中沒有對應的 entry，跳過驗證
	return nil
}

// BinaryNames 回傳當前平台的 binary 名稱列表。
func BinaryNames() []string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return []string{
		"radb" + ext,
	}
}
