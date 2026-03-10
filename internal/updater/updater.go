// Package updater 提供從 GitHub Releases 自動更新 radb 執行檔的功能。
//
// 完整的自動更新流程如下：
//  1. Check — 查詢 GitHub Releases API 取得最新版本，與本機版本比較
//  2. Download — 下載對應平台的 archive（.tar.gz 或 .zip）
//  3. Verify — 下載 checksums.txt 並比對 SHA256，確保檔案完整性
//  4. Extract — 解壓 archive，只提取白名單中的 binary（見 archive.go）
//  5. Replace — 以平台專屬策略替換執行檔（見 replace.go）
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
// Source 欄位為可替換的版本來源介面，方便單元測試時注入 mock。
type Updater struct {
	Source Source
}

// NewUpdater 建立使用 GitHub Releases 作為版本來源的 Updater。
func NewUpdater() *Updater {
	return &Updater{Source: NewGitHubSource()}
}

// CheckResult 包含版本檢查的結果。
type CheckResult struct {
	CurrentVersion string // 本機目前的版本（來自 buildinfo.Version）
	LatestVersion  string // GitHub 上最新的 release tag
	HasUpdate      bool   // 是否有可用的更新
	AssetName      string // 對應平台的 asset 檔名（例如 "radb-v0.2.0-linux-amd64.tar.gz"）
}

// Check 檢查是否有新版本可用。
// 版本判定邏輯：
//   - 若本機版本為 "dev"（開發中建置），只要遠端不是 "dev" 就視為有更新，
//     以便開發者能方便地切換至正式版。
//   - 否則使用 semver 比較（CompareVersions），僅當遠端版本號大於本機版本時才回報更新。
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

// Update 下載並安裝最新版本，執行完整的 Check → Download → Verify → Extract → Replace 流程。
// 若已是最新版本（且非 dev），則直接回傳 HasUpdate=false，不會進行任何檔案操作。
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

	// 若非 dev 且本機版本已 >= 遠端版本，無需更新
	if current != "dev" && CompareVersions(current, info.TagName) >= 0 {
		result.HasUpdate = false
		return result, nil
	}
	result.HasUpdate = true

	// === 階段 1: Download ===
	// 在系統暫存區建立獨立目錄，完成後自動清除
	tmpDir, err := os.MkdirTemp("", "radb-update-*")
	if err != nil {
		return nil, fmt.Errorf("建立暫存目錄失敗: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, info.AssetName)
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		return nil, fmt.Errorf("建立暫存檔案失敗: %w", err)
	}

	// 使用 io.MultiWriter 讓下載的位元組同時寫入檔案和 SHA256 雜湊計算，
	// 避免二次讀取檔案，提升效能
	hash := sha256.New()
	writer := io.MultiWriter(archiveFile, hash)

	if err := u.Source.DownloadAsset(ctx, info.AssetURL, writer); err != nil {
		archiveFile.Close()
		return nil, fmt.Errorf("下載失敗: %w", err)
	}
	archiveFile.Close()

	// === 階段 2: Verify ===
	// 若 release 中附帶 checksums.txt，則下載並比對 SHA256 確保檔案完整
	if info.ChecksumURL != "" {
		if err := u.verifyChecksum(ctx, info, hex.EncodeToString(hash.Sum(nil))); err != nil {
			return nil, err
		}
	}

	// === 階段 3: Extract ===
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

	// === 階段 4: Replace ===
	// 解析目前執行檔的真實路徑（跟隨 symlink），以此判斷安裝目錄
	selfPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("取得執行檔路徑失敗: %w", err)
	}
	selfPath, err = filepath.EvalSymlinks(selfPath)
	if err != nil {
		return nil, fmt.Errorf("解析執行檔路徑失敗: %w", err)
	}
	installDir := filepath.Dir(selfPath)

	// 只替換安裝目錄中已存在的 binary，不會新增不存在的檔案
	for _, newBin := range extracted {
		name := filepath.Base(newBin)
		targetPath := filepath.Join(installDir, name)

		if _, err := os.Stat(targetPath); os.IsNotExist(err) {
			continue
		}

		if err := ReplaceBinary(targetPath, newBin); err != nil {
			return nil, fmt.Errorf("替換 %s 失敗: %w", name, err)
		}
	}

	return result, nil
}

// verifyChecksum 下載 checksums.txt 並驗證 archive 的 SHA256 雜湊值。
//
// checksums.txt 的格式為每行一筆：
//
//	<sha256hex>  <filename>
//
// 流程：
//  1. 下載 checksums.txt 到記憶體（使用 strings.Builder）
//  2. 逐行掃描，找到與 info.AssetName 匹配的行
//  3. 比對該行的雜湊值與下載時計算的 actualHash
//  4. 若找不到對應的 entry（例如舊版 release 沒有此欄位），寬鬆處理，不阻擋更新
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
		// fields[0] = SHA256 hex, fields[1] = 檔名
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
// Windows 平台會自動加上 ".exe" 副檔名。
// 目前只有單一 binary "radb"，但保留 slice 回傳值以便未來擴充。
func BinaryNames() []string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return []string{
		"radb" + ext,
	}
}
