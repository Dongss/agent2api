package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

// maxAsset bounds what a release download may be. The binary is a few tens of
// megabytes; the cap exists so a redirect to something enormous cannot be read
// into memory unbounded, not because any real asset comes close.
const maxAsset = 256 << 20

func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAsset+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxAsset {
		return nil, fmt.Errorf("the download exceeds %d bytes", maxAsset)
	}
	return body, nil
}

// verify fails closed, unlike the install scripts. See the package comment for
// why the two differ.
func verify(archive, sums []byte, name string) error {
	var want string
	for line := range strings.SplitSeq(string(sums), "\n") {
		// checksums.txt is `<sha256>  <name>`; the name carries a leading `*`
		// when it was hashed in binary mode.
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt does not list %s, so the download cannot be verified", name)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", name, want, got)
	}
	return nil
}

func extract(archive []byte, name string) ([]byte, error) {
	if strings.HasSuffix(name, ".zip") {
		return fromZip(archive)
	}
	return fromTarGz(archive)
}

func fromTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("cannot read the release archive: %w", err)
	}
	defer gz.Close()

	want := binaryInArchive()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read the release archive: %w", err)
		}
		// Match on the base name only, and never join the archive's path to
		// anything on disk: nothing here is extracted to a path the archive
		// chose.
		if header.Typeflag != tar.TypeReg || path.Base(header.Name) != want {
			continue
		}
		return readBinary(tr)
	}
	return nil, fmt.Errorf("the release archive does not contain %s", want)
}

func fromZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("cannot read the release archive: %w", err)
	}
	want := binaryInArchive()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || path.Base(f.Name) != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("cannot read %s from the release archive: %w", want, err)
		}
		defer rc.Close()
		return readBinary(rc)
	}
	return nil, fmt.Errorf("the release archive does not contain %s", want)
}

func readBinary(r io.Reader) ([]byte, error) {
	bin, err := io.ReadAll(io.LimitReader(r, maxAsset+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s from the release archive: %w", binaryInArchive(), err)
	}
	if len(bin) > maxAsset {
		return nil, fmt.Errorf("%s in the release archive exceeds %d bytes", binaryInArchive(), maxAsset)
	}
	if len(bin) == 0 {
		return nil, fmt.Errorf("%s in the release archive is empty", binaryInArchive())
	}
	return bin, nil
}
