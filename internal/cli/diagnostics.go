package cli

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/z-chenhao/eri/internal/config"
	"github.com/z-chenhao/eri/internal/observability"
)

const diagnosticFileLimit = 8 * 1024 * 1024

func foregroundLogWriter(output io.Writer) io.Writer {
	file, ok := output.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return nil
	}
	return output
}

type logOptions struct {
	follow bool
	lines  int
	taskID string
}

func runLogs(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) int {
	options, err := parseLogOptions(args)
	if err != nil {
		fmt.Fprintln(stderr, "eri logs:", err)
		return 2
	}
	path := filepath.Join(cfg.DataRoot, "logs", "daemon.log")
	offset, err := printLogTail(path, options.lines, options.taskID, stdout)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, "eri logs: no daemon log exists yet; start `eri daemon` first")
		} else {
			fmt.Fprintln(stderr, "eri logs:", observability.SafeError(err))
		}
		return 1
	}
	if !options.follow {
		return 0
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			next, err := printLogGrowth(path, offset, options.taskID, stdout)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(stderr, "eri logs:", observability.SafeError(err))
				return 1
			}
			if err == nil {
				offset = next
			}
		}
	}
}

func parseLogOptions(args []string) (logOptions, error) {
	options := logOptions{lines: 200}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--follow", "-f":
			options.follow = true
		case "--lines", "-n":
			if index+1 >= len(args) {
				return logOptions{}, fmt.Errorf("%s requires a positive number", args[index])
			}
			index++
			value, err := strconv.Atoi(args[index])
			if err != nil || value <= 0 || value > 10000 {
				return logOptions{}, fmt.Errorf("lines must be between 1 and 10000")
			}
			options.lines = value
		case "--task-id":
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
				return logOptions{}, fmt.Errorf("--task-id requires an id")
			}
			index++
			options.taskID = strings.TrimSpace(args[index])
		default:
			return logOptions{}, fmt.Errorf("unknown option %q", args[index])
		}
	}
	return options, nil
}

func printLogTail(path string, limit int, taskID string, output io.Writer) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	lines := make([]string, 0, limit)
	scanner := bufio.NewScanner(io.LimitReader(file, diagnosticFileLimit))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if taskID != "" && !strings.Contains(line, taskID) {
			continue
		}
		if len(lines) == limit {
			copy(lines, lines[1:])
			lines[len(lines)-1] = line
		} else {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	for _, line := range lines {
		fmt.Fprintln(output, observability.SafeText(line, 1024*1024))
	}
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func printLogGrowth(path string, offset int64, taskID string, output io.Writer) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return offset, err
	}
	if info.Size() < offset {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if taskID == "" || strings.Contains(line, taskID) {
			fmt.Fprintln(output, observability.SafeText(line, 1024*1024))
		}
	}
	if err := scanner.Err(); err != nil {
		return offset, err
	}
	position, err := file.Seek(0, io.SeekCurrent)
	return position, err
}

func runDiagnose(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) int {
	outputPath, err := diagnosticOutputPath(cfg.DataRoot, args)
	if err != nil {
		fmt.Fprintln(stderr, "eri diagnose:", err)
		return 2
	}
	if err := writeDiagnosticBundle(ctx, cfg, outputPath); err != nil {
		fmt.Fprintln(stderr, "eri diagnose:", observability.SafeError(err))
		return 1
	}
	fmt.Fprintln(stdout, "Redacted diagnostic bundle:", outputPath)
	fmt.Fprintln(stdout, "Review the archive before sharing it.")
	return 0
}

func diagnosticOutputPath(dataRoot string, args []string) (string, error) {
	if len(args) > 2 || len(args) == 1 || (len(args) == 2 && args[0] != "--output") {
		return "", fmt.Errorf("usage: eri diagnose [--output PATH]")
	}
	if len(args) == 2 {
		path := strings.TrimSpace(args[1])
		if path == "" {
			return "", fmt.Errorf("--output requires a path")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve output path: %w", err)
		}
		return absolute, nil
	}
	directory := filepath.Join(dataRoot, "exports")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(directory, "eri-diagnostics-"+time.Now().UTC().Format("20060102T150405Z")+".zip"), nil
}

func writeDiagnosticBundle(ctx context.Context, cfg config.Config, outputPath string) (resultErr error) {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			_ = os.Remove(outputPath)
		}
	}()
	archive := zip.NewWriter(file)
	defer func() {
		resultErr = errors.Join(resultErr, archive.Close(), file.Close())
	}()

	manifest, _ := json.MarshalIndent(map[string]any{
		"format": "eri-diagnostic", "created_at": time.Now().UTC(),
		"notice": "Redacted operational metadata only; review before sharing.",
	}, "", "  ")
	if err := addDiagnosticBytes(archive, "manifest.json", manifest); err != nil {
		return err
	}
	configuration, _ := json.MarshalIndent(map[string]any{
		"model_provider": cfg.ModelProvider, "model": observability.SafeText(cfg.Model, 240),
		"raw_model_debug":        cfg.DebugLog,
		"memory_embedding_model": observability.SafeText(cfg.MemoryEmbeddingModel, 240),
		"conversation_address":   cfg.ConversationAddr, "observatory_address": cfg.ObservatoryAddr,
		"model_configured": cfg.ModelConfigured, "lark_enabled": cfg.LarkEnabled, "lark_brand": cfg.LarkBrand,
		"web_enabled": cfg.TavilyKeySet, "web_provider": "tavily",
		"web_search_depth": cfg.TavilySearchDepth, "web_extract_depth": cfg.TavilyExtractDepth,
	}, "", "  ")
	if err := addDiagnosticBytes(archive, "configuration.json", configuration); err != nil {
		return err
	}
	var doctorOut, doctorErr bytes.Buffer
	doctorCode := runDoctor(ctx, cfg, &doctorOut, &doctorErr)
	doctor := fmt.Sprintf("exit_code: %d\n%s%s", doctorCode, doctorOut.String(), doctorErr.String())
	if err := addDiagnosticBytes(archive, "doctor.txt", []byte(redactDiagnosticText(doctor))); err != nil {
		return err
	}

	logDirectory := filepath.Join(cfg.DataRoot, "logs")
	for _, name := range diagnosticLogNames() {
		body, err := os.ReadFile(filepath.Join(logDirectory, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(body) > diagnosticFileLimit {
			body = body[len(body)-diagnosticFileLimit:]
		}
		if err := addDiagnosticBytes(archive, filepath.Join("logs", name), []byte(redactDiagnosticLog(name, string(body)))); err != nil {
			return err
		}
	}
	return nil
}

func diagnosticLogNames() []string {
	result := []string{"bootstrap.log", "provider-secret-broker-bootstrap.log", "google-auth-broker-bootstrap.log"}
	for _, base := range []string{"daemon.log", "provider-secret-broker.log", "google-auth-broker.log"} {
		result = append(result, base)
		for backup := 1; backup <= 3; backup++ {
			result = append(result, fmt.Sprintf("%s.%d", base, backup))
		}
	}
	return result
}

func redactDiagnosticText(body string) string {
	var result strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if isRawProviderDebugRecord(scanner.Text()) {
			continue
		}
		result.WriteString(observability.SafeText(scanner.Text(), 1024*1024))
		result.WriteByte('\n')
	}
	return result.String()
}

func redactDiagnosticLog(name, body string) string {
	if strings.Contains(name, "bootstrap") {
		return redactDiagnosticText(body)
	}
	var result strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), diagnosticFileLimit+1)
	for scanner.Scan() {
		line := scanner.Text()
		var record map[string]any
		// Structured process logs are JSONL. A malformed first line can be a
		// tail fragment of an oversized raw debug record, so fail closed instead
		// of copying potentially private bytes into a shareable archive.
		if json.Unmarshal([]byte(line), &record) != nil || isRawProviderDebugRecord(line) {
			continue
		}
		result.WriteString(observability.SafeText(line, diagnosticFileLimit))
		result.WriteByte('\n')
	}
	return result.String()
}

func isRawProviderDebugRecord(line string) bool {
	var record struct {
		Message string `json:"msg"`
	}
	return json.Unmarshal([]byte(line), &record) == nil && strings.HasPrefix(record.Message, "raw model provider ")
}

func addDiagnosticBytes(archive *zip.Writer, name string, body []byte) error {
	header := &zip.FileHeader{Name: filepath.ToSlash(name), Method: zip.Deflate}
	header.SetMode(0o600)
	writer, err := archive.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = writer.Write(body)
	return err
}
