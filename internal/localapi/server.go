// Package localapi exposes explicit local boundaries for Conversation Workspace,
// System Observatory, and the CLI Unix socket.
package localapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/z-chenhao/eri/internal/a2a"
	"github.com/z-chenhao/eri/internal/agui"
	"github.com/z-chenhao/eri/internal/approval"
	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/episode"
	"github.com/z-chenhao/eri/internal/eventlog"
	"github.com/z-chenhao/eri/internal/evolution"
	"github.com/z-chenhao/eri/internal/observability"
	"github.com/z-chenhao/eri/internal/plugin"
	assistanttask "github.com/z-chenhao/eri/internal/task"
)

type Application interface {
	Connect(context.Context, string, channel.ConnectRequest) (channel.ConnectResult, error)
	Send(context.Context, string, string) (channel.SendResult, error)
	SendWithAttachments(context.Context, string, string, []channel.AttachmentUpload) (channel.SendResult, error)
	Messages(context.Context, int64, int64, int) ([]channel.Message, error)
	TaskMessages(context.Context, string) ([]channel.Message, error)
	Search(context.Context, string, int) ([]channel.Message, error)
	Task(context.Context, string) (channel.TaskStatus, error)
	CurrentPresence(context.Context) (channel.Presence, error)
	Events(context.Context, int64, int) ([]eventlog.Event, error)
	DecideApproval(context.Context, string, approval.Decision) (approval.Result, error)
	Attachment(context.Context, string) (channel.AttachmentContent, bool, error)
}

// ConversationApplication is the complete contract for the Conversation
// surface. Its safe observation projection is mandatory rather than a runtime
// feature probe because every Eri binary ships that surface.
type ConversationApplication interface {
	Application
	ConversationActivity(context.Context, int) (observability.ConversationActivity, error)
	ConversationTrace(context.Context, string) (observability.ConversationTrace, bool, error)
}

// ObservatoryApplication is the complete contract for the developer surface.
// A route must not be registered when its application capability can only fail
// later with a synthetic "not implemented" response.
type ObservatoryApplication interface {
	Application
	Runs(context.Context, int) ([]observability.RunSummary, error)
	Run(context.Context, string) (observability.RunDetail, bool, error)
	Episodes(context.Context, int) ([]episode.Record, error)
	Episode(context.Context, string) (episode.Manifest, bool, error)
	ExportEpisode(context.Context, string) (episode.Manifest, bool, error)
	PromoteEpisode(context.Context, string) (episode.DatasetCandidate, error)
	DatasetSnapshots(context.Context, int) ([]episode.DatasetSnapshot, error)
	FreezeDataset(context.Context, string, string, []string) (episode.DatasetSnapshot, error)
	DatasetSnapshot(context.Context, string) (episode.DatasetSnapshotManifest, bool, error)
	Plugins(context.Context) ([]plugin.Record, error)
	EvolutionReleases(context.Context, int) ([]evolution.Release, error)
	RollbackEvolution(context.Context, string) error
	CancelTask(context.Context, string) (assistanttask.CancelResult, error)
	RetryTask(context.Context, string) (assistanttask.RetryResult, error)
	MemoryOverview(context.Context, int) (observability.MemoryOverview, error)
}

type authMode int

const (
	authConversation authMode = iota
	authObservatory
	authLocalSocket
)

type Server struct {
	app          Application
	conversation ConversationApplication
	observatory  ObservatoryApplication
	mode         authMode
	secret       string
	cookieName   string
	assets       fs.FS
	logger       *slog.Logger
	stop         func()
}

func NewConversation(app ConversationApplication, assets fs.FS, logger *slog.Logger) (*Server, error) {
	server, err := newServer(app, authConversation, "eri_conversation_session", assets, logger)
	if err == nil {
		server.conversation = app
	}
	return server, err
}

func NewObservatory(app ObservatoryApplication, assets fs.FS, logger *slog.Logger) (*Server, error) {
	server, err := newServer(app, authObservatory, "eri_observatory_session", assets, logger)
	if err == nil {
		server.observatory = app
	}
	return server, err
}

func NewLocalSocket(app Application, logger *slog.Logger) (*Server, error) {
	return newServer(app, authLocalSocket, "", nil, logger)
}

func newServer(app Application, mode authMode, cookieName string, assets fs.FS, logger *slog.Logger) (*Server, error) {
	if app == nil {
		return nil, fmt.Errorf("local API application is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	secret := ""
	if mode != authLocalSocket {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generate local session secret: %w", err)
		}
		secret = base64.RawURLEncoding.EncodeToString(raw)
	}
	return &Server{app: app, mode: mode, secret: secret, cookieName: cookieName, assets: assets, logger: logger}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /api/v1/messages", s.protected(s.messages))
	mux.HandleFunc("POST /api/v1/messages", s.protected(s.send))
	if s.mode != authObservatory {
		mux.HandleFunc("POST /api/v1/conversation/connect", s.protected(s.connectConversation))
	}
	mux.HandleFunc("GET /api/v1/search", s.protected(s.search))
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.protected(s.task))
	mux.HandleFunc("GET /api/v1/tasks/{id}/messages", s.protected(s.taskMessages))
	mux.HandleFunc("GET /api/v1/presence", s.protected(s.presence))
	mux.HandleFunc("GET /api/v1/events", s.protected(s.events))
	mux.HandleFunc("POST /api/v1/approvals/{id}", s.protected(s.decideApproval))
	mux.HandleFunc("GET /api/v1/attachments/{id}", s.protected(s.attachment))
	if s.mode == authLocalSocket {
		mux.HandleFunc("POST /api/v1/system/stop", s.protected(s.stopDaemon))
	}
	if s.mode == authConversation {
		mux.HandleFunc("GET /api/v1/activity", s.protected(s.conversationActivity))
		mux.HandleFunc("GET /api/v1/traces/{task_id}", s.protected(s.conversationTrace))
	}
	if s.mode == authObservatory {
		mux.HandleFunc("GET /api/v1/system/overview", s.protected(s.systemOverview))
		mux.HandleFunc("GET /api/v1/runs", s.protected(s.runs))
		mux.HandleFunc("GET /api/v1/runs/{id}", s.protected(s.run))
		mux.HandleFunc("GET /api/v1/memory", s.protected(s.memoryOverview))
		mux.HandleFunc("GET /api/v1/episodes", s.protected(s.episodes))
		mux.HandleFunc("GET /api/v1/episodes/{id}", s.protected(s.episode))
		mux.HandleFunc("POST /api/v1/episodes/{id}/export", s.protected(s.exportEpisode))
		mux.HandleFunc("POST /api/v1/episodes/{id}/dataset-candidate", s.protected(s.promoteEpisode))
		mux.HandleFunc("GET /api/v1/datasets/snapshots", s.protected(s.datasetSnapshots))
		mux.HandleFunc("POST /api/v1/datasets/snapshots", s.protected(s.freezeDataset))
		mux.HandleFunc("GET /api/v1/datasets/snapshots/{id}", s.protected(s.datasetSnapshot))
		mux.HandleFunc("GET /api/v1/plugins", s.protected(s.plugins))
		mux.HandleFunc("GET /api/v1/evolution/releases", s.protected(s.evolutionReleases))
		mux.HandleFunc("POST /api/v1/evolution/releases/{id}/rollback", s.protected(s.rollbackEvolution))
		mux.HandleFunc("GET /api/v1/protocols/ag-ui/events", s.protected(s.aguiEvents))
		mux.HandleFunc("GET /api/v1/protocols/a2a/events", s.protected(s.a2aEvents))
		mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.protected(s.cancelTask))
		mux.HandleFunc("POST /api/v1/tasks/{id}/retry", s.protected(s.retryTask))
	}
	if s.mode != authLocalSocket && s.assets != nil {
		files := http.FileServer(http.FS(s.assets))
		mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !validLoopbackHost(r.Host) {
				http.Error(w, "invalid host", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: s.cookieName, Value: s.secret, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: false,
			})
			w.Header().Set("Cache-Control", "no-store")
			files.ServeHTTP(w, r)
		}))
	}
	return securityHeaders(mux)
}

func (s *Server) connectConversation(w http.ResponseWriter, r *http.Request) {
	var request channel.ConnectRequest
	if err := decodeLocalJSON(w, r, 8*1024, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "The connection context is invalid.")
		return
	}
	result, err := s.app.Connect(r.Context(), s.sourceChannel(), request)
	if err != nil {
		var input *channel.InputError
		if errors.As(err, &input) {
			writeError(w, http.StatusBadRequest, "invalid_input", input.Message)
			return
		}
		s.internalError(w, "connect conversation", err)
		return
	}
	s.logger.Info("conversation connection recorded", "component", "local_api", "source_channel", s.sourceChannel(), "introduction_started", result.IntroductionStarted, "task_id", result.TaskID)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) conversationActivity(w http.ResponseWriter, r *http.Request) {
	activity, err := s.conversation.ConversationActivity(r.Context(), intQuery(r, "limit", 80))
	if err != nil {
		s.internalError(w, "inspect conversation activity", err)
		return
	}
	writeJSON(w, http.StatusOK, activity)
}

func (s *Server) conversationTrace(w http.ResponseWriter, r *http.Request) {
	trace, found, err := s.conversation.ConversationTrace(r.Context(), r.PathValue("task_id"))
	if err != nil {
		s.internalError(w, "inspect conversation trace", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "invalid_input", "Conversation trace not found.")
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

func (s *Server) memoryOverview(w http.ResponseWriter, r *http.Request) {
	overview, err := s.observatory.MemoryOverview(r.Context(), intQuery(r, "limit", 200))
	if err != nil {
		s.internalError(w, "inspect memory", err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	result, err := s.observatory.CancelTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) retryTask(w http.ResponseWriter, r *http.Request) {
	result, err := s.observatory.RetryTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) plugins(w http.ResponseWriter, r *http.Request) {
	records, err := s.observatory.Plugins(r.Context())
	if err != nil {
		s.internalError(w, "list plugins", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": records})
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	runs, err := s.observatory.Runs(r.Context(), intQuery(r, "limit", 100))
	if err != nil {
		s.internalError(w, "list runs", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	detail, found, err := s.observatory.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, "inspect run", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "invalid_input", "Run not found.")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) episodes(w http.ResponseWriter, r *http.Request) {
	records, err := s.observatory.Episodes(r.Context(), intQuery(r, "limit", 100))
	if err != nil {
		s.internalError(w, "list episodes", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"episodes": records})
}

func (s *Server) episode(w http.ResponseWriter, r *http.Request) {
	manifest, found, err := s.observatory.Episode(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, "inspect episode", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "invalid_input", "Episode not found.")
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

func (s *Server) exportEpisode(w http.ResponseWriter, r *http.Request) {
	manifest, found, err := s.observatory.ExportEpisode(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, "export episode", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "invalid_input", "Episode not found.")
		return
	}
	filename := "eri-episode-" + safeFilenameComponent(r.PathValue("id")) + ".json"
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	writeJSON(w, http.StatusOK, manifest)
}

func safeFilenameComponent(value string) string {
	const maxLength = 80
	var safe strings.Builder
	for _, character := range value {
		if safe.Len() >= maxLength {
			break
		}
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			safe.WriteRune(character)
			continue
		}
		if safe.Len() == 0 || safe.String()[safe.Len()-1] != '_' {
			safe.WriteByte('_')
		}
	}
	if safe.Len() == 0 {
		return "unknown"
	}
	return safe.String()
}

func (s *Server) promoteEpisode(w http.ResponseWriter, r *http.Request) {
	candidate, err := s.observatory.PromoteEpisode(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, candidate)
}

func (s *Server) datasetSnapshots(w http.ResponseWriter, r *http.Request) {
	snapshots, err := s.observatory.DatasetSnapshots(r.Context(), intQuery(r, "limit", 100))
	if err != nil {
		s.internalError(w, "list dataset snapshots", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snapshots})
}

func (s *Server) freezeDataset(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Purpose      string   `json:"purpose"`
		SplitSeed    string   `json:"split_seed"`
		CandidateIDs []string `json:"candidate_ids"`
	}
	if err := decodeLocalJSON(w, r, 2*1024*1024, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "The dataset snapshot request is invalid.")
		return
	}
	snapshot, err := s.observatory.FreezeDataset(r.Context(), request.Purpose, request.SplitSeed, request.CandidateIDs)
	if err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, snapshot)
}

func (s *Server) datasetSnapshot(w http.ResponseWriter, r *http.Request) {
	manifest, found, err := s.observatory.DatasetSnapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, "inspect dataset snapshot", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "invalid_input", "Dataset snapshot not found.")
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

func (s *Server) evolutionReleases(w http.ResponseWriter, r *http.Request) {
	releases, err := s.observatory.EvolutionReleases(r.Context(), intQuery(r, "limit", 20))
	if err != nil {
		s.internalError(w, "list evolution releases", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": releases})
}

func (s *Server) rollbackEvolution(w http.ResponseWriter, r *http.Request) {
	if err := s.observatory.RollbackEvolution(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"release_id": r.PathValue("id"), "status": "retired"})
}

func (s *Server) aguiEvents(w http.ResponseWriter, r *http.Request) {
	threadID := strings.TrimSpace(r.URL.Query().Get("thread_id"))
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if threadID == "" || taskID == "" || len(threadID) > 256 || len(taskID) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_input", "Bounded thread_id and task_id values are required.")
		return
	}
	events, err := s.app.Events(r.Context(), int64Query(r, "after", 0), intQuery(r, "limit", 100))
	if err != nil {
		s.internalError(w, "project AG-UI events", err)
		return
	}
	projected := make([]agui.Event, 0, len(events))
	for _, event := range events {
		if !eventBelongsToTask(event, taskID) {
			continue
		}
		projected = append(projected, agui.Project(event, agui.Context{
			ThreadID: threadID, RunID: strings.TrimSpace(r.URL.Query().Get("run_id")), Exposure: "developer",
		})...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": projected})
}

func (s *Server) a2aEvents(w http.ResponseWriter, r *http.Request) {
	contextID := strings.TrimSpace(r.URL.Query().Get("context_id"))
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if contextID == "" || taskID == "" || len(contextID) > 256 || len(taskID) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_input", "Bounded context_id and task_id values are required.")
		return
	}
	events, err := s.app.Events(r.Context(), int64Query(r, "after", 0), intQuery(r, "limit", 100))
	if err != nil {
		s.internalError(w, "project A2A events", err)
		return
	}
	projector := a2a.Projector{}
	projected := make([]a2a.StreamResponse, 0, len(events))
	for _, event := range events {
		if event.AggregateType != "task" || event.AggregateID != taskID {
			continue
		}
		items, err := projector.Project(r.Context(), event, a2a.Context{ContextID: contextID, TaskID: taskID})
		if err != nil {
			s.internalError(w, "project A2A event", err)
			return
		}
		projected = append(projected, items...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": projected, "artifact_projection": false})
}

func eventBelongsToTask(event eventlog.Event, taskID string) bool {
	if event.AggregateType == "task" && event.AggregateID == taskID {
		return true
	}
	value, _ := event.Data["task_id"].(string)
	return value == taskID
}

// SetStopHandler installs the daemon lifecycle command on the privileged Unix
// socket surface. Conversation and Observatory servers never expose it.
func (s *Server) SetStopHandler(stop func()) { s.stop = stop }

func (s *Server) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.mode == authLocalSocket {
			next(w, r)
			return
		}
		if !validLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "unauthorized", "The local Host header is not allowed.")
			return
		}
		cookie, err := r.Cookie(s.cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.secret)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Open this Eri page again to establish a local session.")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameLoopbackOrigin(origin, r.Host) {
			writeError(w, http.StatusForbidden, "unauthorized", "The request origin is not allowed.")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Eri-CSRF") != "1" {
			writeError(w, http.StatusForbidden, "unauthorized", "The local CSRF proof is missing.")
			return
		}
		next(w, r)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) stopDaemon(w http.ResponseWriter, _ *http.Request) {
	if s.stop == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Daemon stop is unavailable.")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "stopping"})
	go s.stop()
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	after := int64Query(r, "after", 0)
	before := int64(-1)
	if _, present := r.URL.Query()["before"]; present {
		before = int64Query(r, "before", 0)
	}
	limit := intQuery(r, "limit", 100)
	messages, err := s.app.Messages(r.Context(), after, before, limit)
	if err != nil {
		s.internalError(w, "list messages", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		text, attachments, err := readMultipartMessage(r)
		if err != nil {
			s.logInboundRejection("multipart_invalid")
			writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		result, err := s.app.SendWithAttachments(r.Context(), s.sourceChannel(), text, attachments)
		if err != nil {
			s.handleSendError(w, "accept multipart message", err)
			return
		}
		s.logger.Info("inbound message accepted", "component", "local_api", "source_channel", s.sourceChannel(), "interaction_id", result.InteractionID, "task_id", result.TaskID, "text_bytes", len([]byte(text)), "attachment_count", len(attachments))
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	var request struct {
		Text string `json:"text"`
	}
	if err := decodeLocalJSON(w, r, 1024*1024+4096, &request); err != nil {
		s.logInboundRejection("json_invalid")
		writeError(w, http.StatusBadRequest, "invalid_input", "The message body is invalid.")
		return
	}
	result, err := s.app.Send(r.Context(), s.sourceChannel(), request.Text)
	if err != nil {
		s.handleSendError(w, "accept message", err)
		return
	}
	s.logger.Info("inbound message accepted", "component", "local_api", "source_channel", s.sourceChannel(), "interaction_id", result.InteractionID, "task_id", result.TaskID, "text_bytes", len([]byte(request.Text)), "attachment_count", 0)
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) sourceChannel() string {
	if s.mode == authLocalSocket {
		return "cli"
	}
	return "conversation_web"
}

func (s *Server) handleSendError(w http.ResponseWriter, operation string, err error) {
	var input *channel.InputError
	if errors.As(err, &input) {
		s.logInboundRejection(input.Code)
		writeError(w, http.StatusBadRequest, "invalid_input", input.Message)
		return
	}
	s.internalError(w, operation, err)
}

func (s *Server) logInboundRejection(reason string) {
	s.logger.Info("inbound message rejected", "component", "local_api", "source_channel", s.sourceChannel(), "reason", reason)
}

func readMultipartMessage(r *http.Request) (string, []channel.AttachmentUpload, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return "", nil, fmt.Errorf("invalid multipart message")
	}
	const maxText = 1024 * 1024
	const maxAttachment = 10 * 1024 * 1024
	const maxTotal = 25 * 1024 * 1024
	var text string
	attachments := make([]channel.AttachmentUpload, 0)
	total := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("read multipart message")
		}
		name := part.FormName()
		filename := part.FileName()
		if filename == "" && name == "text" {
			body, err := io.ReadAll(io.LimitReader(part, maxText+1))
			part.Close()
			if err != nil || len(body) > maxText {
				return "", nil, fmt.Errorf("message field is too large")
			}
			text = string(body)
			continue
		}
		if filename == "" {
			part.Close()
			return "", nil, fmt.Errorf("unexpected multipart field %q", name)
		}
		if name != "files" {
			part.Close()
			return "", nil, fmt.Errorf("unexpected attachment field %q", name)
		}
		if len(attachments) >= 10 {
			part.Close()
			return "", nil, fmt.Errorf("at most 10 attachments are allowed")
		}
		body, err := io.ReadAll(io.LimitReader(part, maxAttachment+1))
		part.Close()
		if err != nil || len(body) == 0 || len(body) > maxAttachment {
			return "", nil, fmt.Errorf("attachment must be between 1 byte and 10 MiB")
		}
		total += len(body)
		if total > maxTotal {
			return "", nil, fmt.Errorf("attachments exceed 25 MiB total")
		}
		cleanName := path.Base(strings.ReplaceAll(filename, "\\", "/"))
		if cleanName == "." || cleanName == "/" || cleanName == "" || strings.ContainsAny(cleanName, "\r\n\x00") {
			return "", nil, fmt.Errorf("attachment filename is invalid")
		}
		mediaType := http.DetectContentType(body)
		if declared, _, err := mime.ParseMediaType(part.Header.Get("Content-Type")); err == nil && declared != "" && mediaType == "application/octet-stream" {
			mediaType = declared
		}
		attachments = append(attachments, channel.AttachmentUpload{Name: cleanName, MediaType: mediaType, Body: body})
	}
	return text, attachments, nil
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	attachment, found, err := s.app.Attachment(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, "read attachment", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "invalid_input", "Attachment not found.")
		return
	}
	w.Header().Set("Content-Type", attachment.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Name}))
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(attachment.Body)), 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write(attachment.Body)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	messages, err := s.app.Search(r.Context(), r.URL.Query().Get("q"), intQuery(r, "limit", 50))
	if err != nil {
		s.internalError(w, "search messages", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Decision approval.Decision `json:"decision"`
	}
	if err := decodeLocalJSON(w, r, 4096, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "The approval decision is invalid.")
		return
	}
	result, err := s.app.DecideApproval(r.Context(), r.PathValue("id"), request.Decision)
	if err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	status, err := s.app.Task(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_input", "Task not found.")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) taskMessages(w http.ResponseWriter, r *http.Request) {
	messages, err := s.app.TaskMessages(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, "list task messages", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

func (s *Server) presence(w http.ResponseWriter, r *http.Request) {
	presence, err := s.app.CurrentPresence(r.Context())
	if err != nil {
		s.internalError(w, "read presence", err)
		return
	}
	writeJSON(w, http.StatusOK, presence)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unavailable", "Streaming is unavailable.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	after := int64Query(r, "after", 0)
	if header := r.Header.Get("Last-Event-ID"); header != "" {
		if parsed, err := strconv.ParseInt(header, 10, 64); err == nil && parsed > after {
			after = parsed
		}
	}
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		events, err := s.app.Events(r.Context(), after, 100)
		if err != nil {
			return
		}
		for _, event := range events {
			var exposed any = event
			if s.mode == authConversation {
				exposed = map[string]any{
					"sequence":   event.Sequence,
					"type":       event.Type,
					"created_at": event.Time,
				}
			}
			encoded, err := json.Marshal(exposed)
			if err != nil {
				return
			}
			fmt.Fprintf(w, "id: %d\nevent: eri\ndata: %s\n\n", event.Sequence, encoded)
			after = event.Sequence
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		if r.URL.Query().Get("once") == "1" {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

type systemComponent struct {
	ID      string         `json:"id"`
	Kind    string         `json:"kind"`
	Label   string         `json:"label"`
	Status  string         `json:"status"`
	Stage   int            `json:"stage"`
	Lane    string         `json:"lane"`
	Metrics map[string]any `json:"metrics"`
	Privacy string         `json:"privacy_summary"`
}

type systemTopologyEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

func (s *Server) systemOverview(w http.ResponseWriter, r *http.Request) {
	presence, err := s.app.CurrentPresence(r.Context())
	if err != nil {
		s.internalError(w, "build system overview", err)
		return
	}
	events, err := s.app.Events(r.Context(), 0, 500)
	if err != nil {
		s.internalError(w, "build system overview", err)
		return
	}
	working := presence.State == "working"
	activeStatus := "idle"
	if working {
		activeStatus = "active"
	}
	modelMetrics := map[string]any{"model_calls": 0, "cache_hit_tokens": 0, "cache_miss_tokens": 0, "cache_hit_rate": "—"}
	cacheHit := 0
	cacheMiss := 0
	modelCalls := 0
	toolCalls := 0
	memoryEvents := 0
	episodes := 0
	feedbackRecords := 0
	installedPlugins, err := s.observatory.Plugins(r.Context())
	if err != nil {
		s.internalError(w, "build plugin overview", err)
		return
	}
	for _, event := range events {
		if event.AggregateType == "effect_intent" || strings.HasPrefix(event.Type, "effect.") {
			toolCalls++
		}
		if event.AggregateType == "memory" {
			memoryEvents++
		}
		if event.Type == "episode.built" {
			episodes++
		}
		if event.Type == "feedback.recorded" {
			feedbackRecords++
		}
		if event.Type != "invocation.succeeded" && event.Type != "invocation.failed" {
			continue
		}
		cacheHit += numberAsInt(event.Data["cache_hit_tokens"])
		cacheMiss += numberAsInt(event.Data["cache_miss_tokens"])
		modelCalls += numberAsInt(event.Data["model_calls"])
		if provider, ok := event.Data["provider"].(string); ok && provider != "" {
			modelMetrics["provider"] = provider
		}
		if model, ok := event.Data["model"].(string); ok && model != "" {
			modelMetrics["model"] = model
		}
	}
	modelMetrics["model_calls"] = modelCalls
	modelMetrics["cache_hit_tokens"] = cacheHit
	modelMetrics["cache_miss_tokens"] = cacheMiss
	if total := cacheHit + cacheMiss; total > 0 {
		modelMetrics["cache_hit_rate"] = fmt.Sprintf("%.1f%%", float64(cacheHit)*100/float64(total))
	}
	components := []systemComponent{
		{ID: "channel", Kind: "channel", Label: "Conversation", Status: "online", Stage: 0, Lane: "primary", Metrics: map[string]any{}, Privacy: "message bodies encrypted"},
		{ID: "runtime", Kind: "runtime", Label: "Durable Runtime", Status: activeStatus, Stage: 1, Lane: "primary", Metrics: map[string]any{"active_tasks": presence.ActiveTasks}, Privacy: "operational metadata"},
		{ID: "memory", Kind: "memory", Label: "Governed Memory", Status: "online", Stage: 1, Lane: "support", Metrics: map[string]any{"events": memoryEvents}, Privacy: "encrypted statements; provenance metadata"},
		{ID: "plugin", Kind: "plugin", Label: "Extension Host", Status: "online", Stage: 2, Lane: "support", Metrics: pluginMetrics(installedPlugins), Privacy: "manifest versions and declared permissions; no credential values"},
		{ID: "agent", Kind: "agent", Label: "Agent Loop", Status: activeStatus, Stage: 2, Lane: "primary", Metrics: map[string]any{}, Privacy: "native messages; no private chain-of-thought"},
		{ID: "model", Kind: "model", Label: "Model Gateway", Status: activeStatus, Stage: 3, Lane: "primary", Metrics: modelMetrics, Privacy: "usage metadata only; prompt bodies remain encrypted"},
		{ID: "tool", Kind: "tool", Label: "Tool Gateway", Status: activeStatus, Stage: 3, Lane: "action", Metrics: map[string]any{"invocations": toolCalls}, Privacy: "parameter hashes and encrypted results"},
		{ID: "eval", Kind: "eval", Label: "LLM Judge + Gates", Status: activeStatus, Stage: 4, Lane: "primary", Metrics: map[string]any{}, Privacy: "encrypted findings; safe result metadata"},
		{ID: "delivery", Kind: "delivery", Label: "Delivery", Status: activeStatus, Stage: 5, Lane: "primary", Metrics: map[string]any{}, Privacy: "receipt metadata"},
		{ID: "feedback", Kind: "feedback", Label: "Outcome Evidence", Status: "online", Stage: 6, Lane: "primary", Metrics: map[string]any{"records": feedbackRecords}, Privacy: "encrypted user statements; causal delivery links"},
		{ID: "episode", Kind: "episode", Label: "Episode Builder", Status: "online", Stage: 6, Lane: "support", Metrics: map[string]any{"episodes": episodes}, Privacy: "encrypted causal manifests without message bodies"},
	}
	topology := []systemTopologyEdge{
		{From: "channel", To: "runtime", Kind: "flow", Label: "request"},
		{From: "runtime", To: "agent", Kind: "flow", Label: "dispatch"},
		{From: "memory", To: "agent", Kind: "support", Label: "context"},
		{From: "agent", To: "model", Kind: "flow", Label: "invoke"},
		{From: "agent", To: "tool", Kind: "flow", Label: "tool call"},
		{From: "plugin", To: "tool", Kind: "support", Label: "capability"},
		{From: "model", To: "eval", Kind: "flow", Label: "candidate"},
		{From: "tool", To: "eval", Kind: "flow", Label: "evidence"},
		{From: "eval", To: "delivery", Kind: "flow", Label: "pass"},
		{From: "delivery", To: "feedback", Kind: "outcome", Label: "outcome"},
		{From: "delivery", To: "episode", Kind: "support", Label: "trace"},
		{From: "feedback", To: "episode", Kind: "support", Label: "evidence"},
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "available", "components": components, "topology": map[string]any{
			"direction": "left_to_right", "edges": topology,
		},
		"metrics": map[string]any{
			"events": len(events), "active_tasks": presence.ActiveTasks,
			"model_calls": modelCalls, "cache_hit_rate": modelMetrics["cache_hit_rate"],
		},
		"generated_at": time.Now().UTC(),
	})
}

func pluginMetrics(records []plugin.Record) map[string]any {
	versions := make(map[string]string, len(records))
	for _, record := range records {
		versions[record.ID] = record.Version
	}
	return map[string]any{"installed": len(records), "versions": versions}
}

func numberAsInt(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func (s *Server) internalError(w http.ResponseWriter, operation string, err error) {
	s.logger.Error("local API failure", "component", "local_api", "operation", operation, "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
	writeError(w, http.StatusInternalServerError, "internal_invariant", "Eri could not complete the local operation.")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func validLoopbackHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func sameLoopbackOrigin(origin, host string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return validLoopbackHost(parsed.Host) && strings.EqualFold(parsed.Host, host)
}

func intQuery(r *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return fallback
	}
	return value
}

func int64Query(r *http.Request, name string, fallback int64) int64 {
	value, err := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func decodeLocalJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain exactly one JSON value")
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}
