package archive

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/vendor/src/code.google.com/p/go/src/pkg/archive/tar"
)

func TestCmdStreamLargeStderr(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "dd if=/dev/zero bs=1k count=1000 of=/dev/stderr; echo hello")
	out, err := CmdStream(cmd, nil)
	if err != nil {
		t.Fatalf("Failed to start command: %s", err)
	}
	errCh := make(chan error)
	go func() {
		_, err := io.Copy(ioutil.Discard, out)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Command should not have failed (err=%.100s...)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Command did not complete in 5 seconds; probable deadlock")
	}
}

func TestCmdStreamBad(t *testing.T) {
	badCmd := exec.Command("/bin/sh", "-c", "echo hello; echo >&2 error couldn\\'t reverse the phase pulser; exit 1")
	out, err := CmdStream(badCmd, nil)
	if err != nil {
		t.Fatalf("Failed to start command: %s", err)
	}
	if output, err := ioutil.ReadAll(out); err == nil {
		t.Fatalf("Command should have failed")
	} else if err.Error() != "exit status 1: error couldn't reverse the phase pulser\n" {
		t.Fatalf("Wrong error value (%s)", err)
	} else if s := string(output); s != "hello\n" {
		t.Fatalf("Command output should be '%s', not '%s'", "hello\\n", output)
	}
}

func TestCmdStreamGood(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "echo hello; exit 0")
	out, err := CmdStream(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := ioutil.ReadAll(out); err != nil {
		t.Fatalf("Command should not have failed (err=%s)", err)
	} else if s := string(output); s != "hello\n" {
		t.Fatalf("Command output should be '%s', not '%s'", "hello\\n", output)
	}
}

func tarUntar(t *testing.T, origin string, options *TarOptions) ([]Change, error) {
	archive, err := TarWithOptions(origin, options)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()

	buf := make([]byte, 10)
	if _, err := archive.Read(buf); err != nil {
		return nil, err
	}
	wrap := io.MultiReader(bytes.NewReader(buf), archive)

	detectedCompression := DetectCompression(buf)
	compression := options.Compression
	if detectedCompression.Extension() != compression.Extension() {
		return nil, fmt.Errorf("Wrong compression detected. Actual compression: %s, found %s", compression.Extension(), detectedCompression.Extension())
	}

	tmp, err := ioutil.TempDir("", "docker-test-untar")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := Untar(wrap, tmp, nil); err != nil {
		return nil, err
	}
	if _, err := os.Stat(tmp); err != nil {
		return nil, err
	}

	return ChangesDirs(origin, tmp, nil)
}

func TestTarUntar(t *testing.T) {
	origin, err := ioutil.TempDir("", "docker-test-untar-origin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(origin)
	if err := ioutil.WriteFile(path.Join(origin, "1"), []byte("hello world"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(path.Join(origin, "2"), []byte("welcome!"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(path.Join(origin, "3"), []byte("will be ignored"), 0700); err != nil {
		t.Fatal(err)
	}

	for _, c := range []Compression{
		Uncompressed,
		Gzip,
	} {
		changes, err := tarUntar(t, origin, &TarOptions{
			Compression: c,
			Excludes:    []string{"3"},
		})

		if err != nil {
			t.Fatalf("Error tar/untar for compression %s: %s", c.Extension(), err)
		}

		if len(changes) != 1 || changes[0].Path != "/3" {
			t.Fatalf("Unexpected differences after tarUntar: %v", changes)
		}
	}
}

func TestTarWithOptions(t *testing.T) {
	origin, err := ioutil.TempDir("", "docker-test-untar-origin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(origin)
	if err := ioutil.WriteFile(path.Join(origin, "1"), []byte("hello world"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(path.Join(origin, "2"), []byte("welcome!"), 0700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		opts       *TarOptions
		numChanges int
	}{
		{&TarOptions{Includes: []string{"1"}}, 1},
		{&TarOptions{Excludes: []string{"2"}}, 1},
	}
	for _, testCase := range cases {
		changes, err := tarUntar(t, origin, testCase.opts)
		if err != nil {
			t.Fatalf("Error tar/untar when testing inclusion/exclusion: %s", err)
		}
		if len(changes) != testCase.numChanges {
			t.Errorf("Expected %d changes, got %d for %+v:",
				testCase.numChanges, len(changes), testCase.opts)
		}
	}
}

// Some tar archives such as http://haproxy.1wt.eu/download/1.5/src/devel/haproxy-1.5-dev21.tar.gz
// use PAX Global Extended Headers.
// Failing prevents the archives from being uncompressed during ADD
func TestTypeXGlobalHeaderDoesNotFail(t *testing.T) {
	hdr := tar.Header{Typeflag: tar.TypeXGlobalHeader}
	tmpDir, err := ioutil.TempDir("", "docker-test-archive-pax-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	err = createTarFile(filepath.Join(tmpDir, "pax_global_header"), tmpDir, &hdr, nil, true)
	if err != nil {
		t.Fatal(err)
	}
}

// Some tar have both GNU specific (huge uid) and Ustar specific (long name) things.
// Not supposed to happen (should use PAX instead of Ustar for long name) but it does and it should still work.
func TestUntarUstarGnuConflict(t *testing.T) {
	f, err := os.Open("testdata/broken.tar")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	tr := tar.NewReader(f)
	// Iterate through the files in the archive.
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// end of tar archive
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "root/.cpanm/work/1395823785.24209/Plack-1.0030/blib/man3/Plack::Middleware::LighttpdScriptNameFix.3pm" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%s not found in the archive", "root/.cpanm/work/1395823785.24209/Plack-1.0030/blib/man3/Plack::Middleware::LighttpdScriptNameFix.3pm")
	}
}

func prepareUntarSourceDirectory(numberOfFiles int, targetPath string) (int, error) {
	fileData := []byte("fooo")
	for n := 0; n < numberOfFiles; n++ {
		fileName := fmt.Sprintf("file-%d", n)
		if err := ioutil.WriteFile(path.Join(targetPath, fileName), fileData, 0700); err != nil {
			return 0, err
		}
	}
	totalSize := numberOfFiles * len(fileData)
	return totalSize, nil
}

func BenchmarkTarUntar(b *testing.B) {
	origin, err := ioutil.TempDir("", "docker-test-untar-origin")
	if err != nil {
		b.Fatal(err)
	}
	tempDir, err := ioutil.TempDir("", "docker-test-untar-destination")
	if err != nil {
		b.Fatal(err)
	}
	target := path.Join(tempDir, "dest")
	n, err := prepareUntarSourceDirectory(100, origin)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.SetBytes(int64(n))
	defer os.RemoveAll(origin)
	defer os.RemoveAll(tempDir)
	for n := 0; n < b.N; n++ {
		err := TarUntar(origin, target)
		if err != nil {
			b.Fatal(err)
		}
		os.RemoveAll(target)
	}
}

func TestUntarInvalidFilenames(t *testing.T) {
	for i, headers := range [][]*tar.Header{
		{
			{
				Name:     "../victim/dotdot",
				Typeflag: tar.TypeReg,
				Mode:     0644,
			},
		},
		{
			{
				// Note the leading slash
				Name:     "/../victim/slash-dotdot",
				Typeflag: tar.TypeReg,
				Mode:     0644,
			},
		},
	} {
		if err := testBreakout("untar", "docker-TestUntarInvalidFilenames", headers); err != nil {
			t.Fatalf("i=%d. %v", i, err)
		}
	}
}

func TestUntarInvalidHardlink(t *testing.T) {
	for i, headers := range [][]*tar.Header{
		{ // try reading victim/hello (../)
			{
				Name:     "dotdot",
				Typeflag: tar.TypeLink,
				Linkname: "../victim/hello",
				Mode:     0644,
			},
		},
		{ // try reading victim/hello (/../)
			{
				Name:     "slash-dotdot",
				Typeflag: tar.TypeLink,
				// Note the leading slash
				Linkname: "/../victim/hello",
				Mode:     0644,
			},
		},
		{ // try writing victim/file
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeLink,
				Linkname: "../victim",
				Mode:     0755,
			},
			{
				Name:     "loophole-victim/file",
				Typeflag: tar.TypeReg,
				Mode:     0644,
			},
		},
		{ // try reading victim/hello (hardlink, symlink)
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeLink,
				Linkname: "../victim",
				Mode:     0755,
			},
			{
				Name:     "symlink",
				Typeflag: tar.TypeSymlink,
				Linkname: "loophole-victim/hello",
				Mode:     0644,
			},
		},
		{ // Try reading victim/hello (hardlink, hardlink)
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeLink,
				Linkname: "../victim",
				Mode:     0755,
			},
			{
				Name:     "hardlink",
				Typeflag: tar.TypeLink,
				Linkname: "loophole-victim/hello",
				Mode:     0644,
			},
		},
		{ // Try removing victim directory (hardlink)
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeLink,
				Linkname: "../victim",
				Mode:     0755,
			},
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeReg,
				Mode:     0644,
			},
		},
	} {
		if err := testBreakout("untar", "docker-TestUntarInvalidHardlink", headers); err != nil {
			t.Fatalf("i=%d. %v", i, err)
		}
	}
}

func TestUntarInvalidSymlink(t *testing.T) {
	for i, headers := range [][]*tar.Header{
		{ // try reading victim/hello (../)
			{
				Name:     "dotdot",
				Typeflag: tar.TypeSymlink,
				Linkname: "../victim/hello",
				Mode:     0644,
			},
		},
		{ // try reading victim/hello (/../)
			{
				Name:     "slash-dotdot",
				Typeflag: tar.TypeSymlink,
				// Note the leading slash
				Linkname: "/../victim/hello",
				Mode:     0644,
			},
		},
		{ // try writing victim/file
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeSymlink,
				Linkname: "../victim",
				Mode:     0755,
			},
			{
				Name:     "loophole-victim/file",
				Typeflag: tar.TypeReg,
				Mode:     0644,
			},
		},
		{ // try reading victim/hello (symlink, symlink)
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeSymlink,
				Linkname: "../victim",
				Mode:     0755,
			},
			{
				Name:     "symlink",
				Typeflag: tar.TypeSymlink,
				Linkname: "loophole-victim/hello",
				Mode:     0644,
			},
		},
		{ // try reading victim/hello (symlink, hardlink)
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeSymlink,
				Linkname: "../victim",
				Mode:     0755,
			},
			{
				Name:     "hardlink",
				Typeflag: tar.TypeLink,
				Linkname: "loophole-victim/hello",
				Mode:     0644,
			},
		},
		{ // try removing victim directory (symlink)
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeSymlink,
				Linkname: "../victim",
				Mode:     0755,
			},
			{
				Name:     "loophole-victim",
				Typeflag: tar.TypeReg,
				Mode:     0644,
			},
		},
		{ // try writing to victim/newdir/newfile with a symlink in the path
			{
				// this header needs to be before the next one, or else there is an error
				Name:     "dir/loophole",
				Typeflag: tar.TypeSymlink,
				Linkname: "../../victim",
				Mode:     0755,
			},
			{
				Name:     "dir/loophole/newdir/newfile",
				Typeflag: tar.TypeReg,
				Mode:     0644,
			},
		},
	} {
		if err := testBreakout("untar", "docker-TestUntarInvalidSymlink", headers); err != nil {
			t.Fatalf("i=%d. %v", i, err)
		}
	}
}
