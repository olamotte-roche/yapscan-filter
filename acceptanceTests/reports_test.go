package acceptanceTests

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/klauspost/compress/zstd"

	"github.com/fkie-cad/yapscan/app"

	. "github.com/smartystreets/goconvey/convey"
)

const maxRandomDataSize = 4095
const maxSizeFilter = "4K"

func TestMain(m *testing.M) {
	closer := initializeMemoryTester()
	defer closer.Close()

	m.Run()
}

func TestMatchIsFound_Simple(t *testing.T) {
	Convey("Scanning a prepared process with full-report on", t, func(c C) {
		yaraRulesPath, pid, addressOfData := withYaraRulesFileAndMatchingMemoryTester(t, c, []byte("hello world"))
		stdout, stderr, cleanupCapture := withCapturedOutput(t)

		args := []string{"yapscan",
			"scan",
			"-r", yaraRulesPath,
			"--filter-size-max", maxSizeFilter,
			strconv.Itoa(pid)}
		ctx, cancel := context.WithTimeout(context.Background(), yapscanTimeout)
		err := app.MakeApp(args).RunContext(ctx, args)
		cancel()

		cleanupCapture()

		conveyMatchWasSuccessful(c, addressOfData, err, stdout, stderr)
	})
}

func TestMatchIsFound_Fuzzy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	i := 0
	fmt.Println()

	f := func(data []byte) bool {
		if len(data) == 0 || len(data) >= maxRandomDataSize {
			return true
		}

		// If there is no output for an extended period of time, travic-ci will just kill the job
		fmt.Printf("\rFuzzy test %4d", i)
		i++
		os.Stdout.Sync()

		yaraRulesPath, pid, addressOfData := withYaraRulesFileAndMatchingMemoryTester(t, nil, data)
		stdout, stderr, cleanupCapture := withCapturedOutput(t)

		args := []string{"yapscan",
			"scan",
			"-r", yaraRulesPath,
			"--filter-size-max", maxSizeFilter,
			strconv.Itoa(pid)}
		ctx, cancel := context.WithTimeout(context.Background(), yapscanTimeout)
		err := app.MakeApp(args).RunContext(ctx, args)
		cancel()

		cleanupCapture()

		return err == nil &&
			stderr.String() == "" &&
			strings.Contains(stdout.String(), fmt.Sprintf("Rule-strings matched at 0x%X.", addressOfData))
	}

	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
}

func TestDoesNotMatchFalsePositive_Fuzzy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	i := 0
	fmt.Println()

	f := func(data []byte) bool {
		if len(data) == 0 || len(data) >= maxRandomDataSize {
			return true
		}

		// If there is no output for an extended period of time, travic-ci will just kill the job
		fmt.Printf("\rFuzzy test %4d", i)
		i++
		os.Stdout.Sync()

		yaraRulesPath, pid, addressOfData := withYaraRulesFileAndNotMatchingMemoryTester(t, nil, data)
		stdout, stderr, cleanupCapture := withCapturedOutput(t)

		args := []string{"yapscan",
			"scan",
			"-r", yaraRulesPath,
			"--filter-size-max", maxSizeFilter,
			strconv.Itoa(pid)}
		ctx, cancel := context.WithTimeout(context.Background(), yapscanTimeout)
		err := app.MakeApp(args).RunContext(ctx, args)
		cancel()

		cleanupCapture()

		return err == nil &&
			stderr.String() == "" &&
			!strings.Contains(stdout.String(), fmt.Sprintf("Rule-strings matched at 0x%X.", addressOfData))
	}

	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
	fmt.Println()
}

func TestFullReportIsWritten_Unencrypted(t *testing.T) {
	Convey("Scanning a prepared process with full-report on", t, func(c C) {
		yaraRulesPath, pid, addressOfData := withYaraRulesFileAndMatchingMemoryTester(t, c, []byte("hello world"))
		stdout, stderr, cleanupCapture := withCapturedOutput(t)

		reportDir := t.TempDir()
		args := []string{"yapscan",
			"scan",
			"-r", yaraRulesPath,
			"--filter-size-max", maxSizeFilter,
			"--full-report", "--report-dir", reportDir,
			strconv.Itoa(pid)}
		ctx, cancel := context.WithTimeout(context.Background(), yapscanTimeout)
		err := app.MakeApp(args).RunContext(ctx, args)
		cancel()

		cleanupCapture()

		conveyMatchWasSuccessful(c, addressOfData, err, stdout, stderr)
		conveyReportIsCleartextReadable(c, reportDir)
	})
}

func findReportPath(reportDir string) (string, bool) {
	var reportName string
	dir, _ := os.ReadDir(reportDir)
	for _, entry := range dir {
		if !entry.IsDir() && strings.Contains(entry.Name(), ".tar.zstd") {
			reportName = entry.Name()
			break
		}
	}
	return filepath.Join(reportDir, reportName), reportName != ""
}

func conveyReportIsCleartextReadable(c C, reportDir string) {
	c.Convey("should be a valid zstd compressed file", func(c C) {
		reportPath, exists := findReportPath(reportDir)

		c.So(exists, ShouldBeTrue)
		if !exists {
			return
		}

		f, _ := os.Open(reportPath)
		defer f.Close()

		reportFiles, err := readReport(c, f)

		c.So(reportFiles, ShouldNotBeEmpty)
		c.So(err, ShouldBeNil)

		filenames := make([]string, len(reportFiles))
		for i, file := range reportFiles {
			filenames[i] = file.Name
		}
		c.Convey("and contain the expected files.", func(c C) {
			c.So(filenames, ShouldContain, "rules.yarc")
			c.So(filenames, ShouldContain, "systeminfo.json")
			c.So(filenames, ShouldContain, "processes.json")
			c.So(filenames, ShouldContain, "memory-scans.json")
			c.So(filenames, ShouldHaveLength, 4)
		})
	})
}

type file struct {
	Name string
	Data []byte
}

func readReport(c C, rdr io.Reader) ([]*file, error) {
	zstdRdr, err := zstd.NewReader(rdr)
	if err != nil {
		return nil, err
	}
	defer zstdRdr.Close()

	result := make([]*file, 0)

	tarRdr := tar.NewReader(zstdRdr)
	for {
		var hdr *tar.Header
		hdr, err = tarRdr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag == tar.TypeReg {
			file := &file{
				Name: filepath.Base(hdr.Name),
			}
			buf := &bytes.Buffer{}
			if _, err = io.Copy(buf, tarRdr); err != nil {
				break
			}
			file.Data = buf.Bytes()

			result = append(result, file)
		}
	}

	if err == io.EOF {
		err = nil
	}

	return result, err
}

func conveyMatchWasSuccessful(c C, addressOfData uintptr, err error, stdout, stderr *bytes.Buffer) {
	c.Convey("should not error and find the correct match.", func() {
		c.So(err, ShouldBeNil)
		c.So(stderr.String(), ShouldBeEmpty)
		c.So(stdout.String(), ShouldContainSubstring, fmt.Sprintf("Rule-strings matched at 0x%X.", addressOfData))
	})
}
