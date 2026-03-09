package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 允許從 archive 中提取的 binary 名稱。
var knownBinaries = map[string]bool{
	"radb":     true,
	"radb.exe": true,
}

// ExtractArchive 解壓 archive 到 destDir，只提取已知的 binary 檔案。
// 根據副檔名自動判斷 tar.gz 或 zip 格式。
// 回傳已提取的檔案路徑列表。
func ExtractArchive(archivePath, destDir string) ([]string, error) {
	if strings.HasSuffix(archivePath, ".tar.gz") || strings.HasSuffix(archivePath, ".tgz") {
		return extractTarGz(archivePath, destDir)
	}
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, destDir)
	}
	return nil, fmt.Errorf("不支援的 archive 格式: %s", archivePath)
}

func extractTarGz(archivePath, destDir string) ([]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("開啟 archive 失敗: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("建立 gzip reader 失敗: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var extracted []string

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("讀取 tar entry 失敗: %w", err)
		}

		// 只處理一般檔案
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(hdr.Name)

		// 安全性：拒絕 path traversal
		if strings.Contains(hdr.Name, "..") {
			continue
		}

		if !knownBinaries[name] {
			continue
		}

		destPath := filepath.Join(destDir, name)
		if err := writeFile(destPath, tr, hdr.FileInfo().Mode()); err != nil {
			return nil, err
		}
		extracted = append(extracted, destPath)
	}

	return extracted, nil
}

func extractZip(archivePath, destDir string) ([]string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("開啟 zip 失敗: %w", err)
	}
	defer zr.Close()

	var extracted []string

	for _, zf := range zr.File {
		name := filepath.Base(zf.Name)

		// 安全性：拒絕 path traversal
		if strings.Contains(zf.Name, "..") {
			continue
		}

		if zf.FileInfo().IsDir() {
			continue
		}

		if !knownBinaries[name] {
			continue
		}

		rc, err := zf.Open()
		if err != nil {
			return nil, fmt.Errorf("開啟 zip entry %q 失敗: %w", zf.Name, err)
		}

		destPath := filepath.Join(destDir, name)
		err = writeFile(destPath, rc, zf.Mode())
		rc.Close()
		if err != nil {
			return nil, err
		}
		extracted = append(extracted, destPath)
	}

	return extracted, nil
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	// 確保至少有執行權限
	if mode == 0 {
		mode = 0755
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("建立檔案 %q 失敗: %w", path, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("寫入檔案 %q 失敗: %w", path, err)
	}
	return nil
}
