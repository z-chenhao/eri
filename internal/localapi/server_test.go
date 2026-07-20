package localapi

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/z-chenhao/eri/internal/approval"
	"github.com/z-chenhao/eri/internal/channel"
	"github.com/z-chenhao/eri/internal/episode"
	"github.com/z-chenhao/eri/internal/eventlog"
	"github.com/z-chenhao/eri/internal/evolution"
	"github.com/z-chenhao/eri/internal/observability"
	"github.com/z-chenhao/eri/internal/plugin"
	assistanttask "github.com/z-chenhao/eri/internal/task"
)

type fakeApplication struct {
	sent           int
	canceled       string
	retried        string
	messagesAfter  int64
	messagesBefore int64
	messagesLimit  int
	sourceChannels []string
	sendErr        error
	events         []eventlog.Event
	connections    []string
	connectResult  channel.ConnectResult
}

func (a *fakeApplication) Connect(_ context.Context, sourceChannel string, _ channel.ConnectRequest) (channel.ConnectResult, error) {
	a.connections = append(a.connections, sourceChannel)
	return a.connectResult, nil
}

func (a *fakeApplication) Send(_ context.Context, sourceChannel, _ string) (channel.SendResult, error) {
	a.sent++
	a.sourceChannels = append(a.sourceChannels, sourceChannel)
	if a.sendErr != nil {
		return channel.SendResult{}, a.sendErr
	}
	return channel.SendResult{InteractionID: "interaction", TaskID: "task"}, nil
}
func (a *fakeApplication) SendWithAttachments(_ context.Context, sourceChannel, _ string, _ []channel.AttachmentUpload) (channel.SendResult, error) {
	a.sent++
	a.sourceChannels = append(a.sourceChannels, sourceChannel)
	if a.sendErr != nil {
		return channel.SendResult{}, a.sendErr
	}
	return channel.SendResult{InteractionID: "interaction", TaskID: "task"}, nil
}
func (a *fakeApplication) Messages(_ context.Context, after, before int64, limit int) ([]channel.Message, error) {
	a.messagesAfter, a.messagesBefore, a.messagesLimit = after, before, limit
	return []channel.Message{}, nil
}
func (*fakeApplication) TaskMessages(_ context.Context, taskID string) ([]channel.Message, error) {
	return []channel.Message{{TaskID: taskID, Direction: "outbound", Content: "done"}}, nil
}
func (*fakeApplication) Search(context.Context, string, int) ([]channel.Message, error) {
	return []channel.Message{}, nil
}
func (*fakeApplication) Task(context.Context, string) (channel.TaskStatus, error) {
	return channel.TaskStatus{ID: "task", Status: "completed"}, nil
}
func (*fakeApplication) CurrentPresence(context.Context) (channel.Presence, error) {
	return channel.Presence{State: "available"}, nil
}
func (a *fakeApplication) Events(context.Context, int64, int) ([]eventlog.Event, error) {
	if a.events != nil {
		return a.events, nil
	}
	event := eventlog.Event{Sequence: 1, ID: "event-private", AggregateType: "task", AggregateID: "task-private", Type: "task.created", Data: map[string]any{"private_detail": "developer-only"}, Time: time.Now()}
	eventlog.Normalize(&event)
	return []eventlog.Event{event}, nil
}
func (*fakeApplication) DecideApproval(_ context.Context, id string, decision approval.Decision) (approval.Result, error) {
	return approval.Result{ApprovalID: id, TaskID: "task", Status: string(decision)}, nil
}
func (*fakeApplication) Attachment(context.Context, string) (channel.AttachmentContent, bool, error) {
	return channel.AttachmentContent{}, false, nil
}

func TestMessagesSupportsLatestAndOlderCursors(t *testing.T) {
	app := &fakeApplication{}
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	server, err := NewConversation(app, assets, nil)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7780/", nil)
	bootstrap.Host = "127.0.0.1:7780"
	bootstrapRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(bootstrapRecorder, bootstrap)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7780/api/v1/messages?before=321&limit=200", nil)
	request.Host = "127.0.0.1:7780"
	request.AddCookie(bootstrapRecorder.Result().Cookies()[0])
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || app.messagesAfter != 0 || app.messagesBefore != 321 || app.messagesLimit != 200 {
		t.Fatalf("status=%d cursors=%d/%d limit=%d", recorder.Code, app.messagesAfter, app.messagesBefore, app.messagesLimit)
	}
}

func TestConversationConnectStartsIntroductionThroughTrustedChannel(t *testing.T) {
	app := &fakeApplication{connectResult: channel.ConnectResult{IntroductionStarted: true, TaskID: "intro-task"}}
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	server, err := NewConversation(app, assets, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie := bootstrapCookie(t, server, "127.0.0.1:7780")
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7780/api/v1/conversation/connect", strings.NewReader(`{"locale":"zh-CN","timezone":"Asia/Shanghai"}`))
	request.Host = "127.0.0.1:7780"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Eri-CSRF", "1")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || len(app.connections) != 1 || app.connections[0] != "conversation_web" || !strings.Contains(recorder.Body.String(), `"introduction_started":true`) {
		t.Fatalf("status=%d connections=%v body=%s", recorder.Code, app.connections, recorder.Body.String())
	}
}

func TestTaskMessagesUsesAnExactTaskScopedEndpoint(t *testing.T) {
	app := &fakeApplication{}
	server, err := NewLocalSocket(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://eri.local/api/v1/tasks/task-500/messages", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"task_id":"task-500"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestInboundInternalFailureIsLoggedButNotReturnedAsValidationDetail(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	privateDetail := fmt.Sprintf("write %s/private.db: token=private-runtime-token", home)
	app := &fakeApplication{sendErr: fmt.Errorf("%s", privateDetail)}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	server, err := NewLocalSocket(app, logger)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://eri.local/api/v1/messages", strings.NewReader(`{"text":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), privateDetail) || !strings.Contains(recorder.Body.String(), "internal_invariant") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	logged := logs.String()
	if !strings.Contains(logged, `"msg":"local API failure"`) || !strings.Contains(logged, "$HOME/private.db") || !strings.Contains(logged, "token=[REDACTED]") || strings.Contains(logged, "private-runtime-token") {
		t.Fatalf("unsafe or incomplete log: %s", logged)
	}
}

func TestDownloadFilenameComponentCannotInjectResponseHeaders(t *testing.T) {
	value := safeFilenameComponent("episode\"\r\nX-Attack: yes/../\u7ed3\u5c3e")
	if strings.ContainsAny(value, "\r\n\"/\\") || value != "episode_X-Attack_yes_.._" {
		t.Fatalf("unsafe filename component = %q", value)
	}
}
func (*fakeApplication) ConversationActivity(context.Context, int) (observability.ConversationActivity, error) {
	return observability.ConversationActivity{
		Active: []observability.ConversationRun{},
		Recent: []observability.ConversationRun{{RunID: "run", TaskID: "task", Status: "completed"}},
	}, nil
}
func (*fakeApplication) ConversationTrace(context.Context, string) (observability.ConversationTrace, bool, error) {
	return observability.ConversationTrace{
		TaskID: "task", RunID: "run", Status: "completed",
		Steps: []observability.RunSpan{{ID: "runtime", Kind: "runtime", Title: "Run accepted"}},
	}, true, nil
}
func (*fakeApplication) MemoryOverview(context.Context, int) (observability.MemoryOverview, error) {
	return observability.MemoryOverview{Total: 1, Observations: []observability.MemoryObservation{{MemoryID: "memory", Stages: []observability.MemoryStage{observability.MemoryStored}}}}, nil
}
func (*fakeApplication) Runs(context.Context, int) ([]observability.RunSummary, error) {
	return []observability.RunSummary{}, nil
}
func (*fakeApplication) Run(context.Context, string) (observability.RunDetail, bool, error) {
	return observability.RunDetail{}, false, nil
}
func (*fakeApplication) Episodes(context.Context, int) ([]episode.Record, error) {
	return []episode.Record{}, nil
}
func (*fakeApplication) Episode(context.Context, string) (episode.Manifest, bool, error) {
	return episode.Manifest{}, false, nil
}
func (*fakeApplication) ExportEpisode(context.Context, string) (episode.Manifest, bool, error) {
	return episode.Manifest{}, false, nil
}
func (*fakeApplication) PromoteEpisode(context.Context, string) (episode.DatasetCandidate, error) {
	return episode.DatasetCandidate{}, nil
}
func (*fakeApplication) DatasetSnapshots(context.Context, int) ([]episode.DatasetSnapshot, error) {
	return []episode.DatasetSnapshot{}, nil
}
func (*fakeApplication) FreezeDataset(context.Context, string, string, []string) (episode.DatasetSnapshot, error) {
	return episode.DatasetSnapshot{}, nil
}
func (*fakeApplication) DatasetSnapshot(context.Context, string) (episode.DatasetSnapshotManifest, bool, error) {
	return episode.DatasetSnapshotManifest{}, false, nil
}
func (*fakeApplication) Plugins(context.Context) ([]plugin.Record, error) {
	return []plugin.Record{}, nil
}
func (*fakeApplication) EvolutionReleases(context.Context, int) ([]evolution.Release, error) {
	return []evolution.Release{}, nil
}
func (*fakeApplication) RollbackEvolution(context.Context, string) error { return nil }
func (a *fakeApplication) CancelTask(_ context.Context, id string) (assistanttask.CancelResult, error) {
	a.canceled = id
	return assistanttask.CancelResult{TaskID: id, Status: "running", Effect: "cancel_requested"}, nil
}
func (a *fakeApplication) RetryTask(_ context.Context, id string) (assistanttask.RetryResult, error) {
	a.retried = id
	return assistanttask.RetryResult{SourceTaskID: id, TaskID: "retry-task", Status: "queued", Checkpoint: "task_start"}, nil
}

func TestConversationSessionRejectsCrossOriginMutation(t *testing.T) {
	t.Parallel()
	app := &fakeApplication{}
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	server, err := NewConversation(app, fs.FS(assets), nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	bootstrap := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7780/", nil)
	bootstrap.Host = "127.0.0.1:7780"
	bootstrapRecorder := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapRecorder, bootstrap)
	if bootstrapRecorder.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d", bootstrapRecorder.Code)
	}
	cookies := bootstrapRecorder.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected bootstrap cookie: %+v", cookies)
	}

	withoutSession := postMessageRequest()
	withoutRecorder := httptest.NewRecorder()
	handler.ServeHTTP(withoutRecorder, withoutSession)
	if withoutRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("without session status = %d", withoutRecorder.Code)
	}

	crossOrigin := postMessageRequest()
	crossOrigin.AddCookie(cookies[0])
	crossOrigin.Header.Set("Origin", "https://attacker.example")
	crossRecorder := httptest.NewRecorder()
	handler.ServeHTTP(crossRecorder, crossOrigin)
	if crossRecorder.Code != http.StatusForbidden || app.sent != 0 {
		t.Fatalf("cross-origin status = %d, sent = %d", crossRecorder.Code, app.sent)
	}

	allowed := postMessageRequest()
	allowed.AddCookie(cookies[0])
	allowed.Header.Set("Origin", "http://127.0.0.1:7780")
	allowedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(allowedRecorder, allowed)
	if allowedRecorder.Code != http.StatusAccepted || app.sent != 1 {
		t.Fatalf("allowed status = %d, sent = %d", allowedRecorder.Code, app.sent)
	}
}

func TestIngressRejectsCallerSuppliedChannel(t *testing.T) {
	t.Parallel()
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	conversationApp := &fakeApplication{}
	conversation, err := NewConversation(conversationApp, assets, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie := bootstrapCookie(t, conversation, "127.0.0.1:7780")
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7780/api/v1/messages", strings.NewReader(`{"text":"hello","channel":"trusted_future_channel"}`))
	request.Host = "127.0.0.1:7780"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Eri-CSRF", "1")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	conversation.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || len(conversationApp.sourceChannels) != 0 {
		t.Fatalf("status=%d source channels=%v", recorder.Code, conversationApp.sourceChannels)
	}

	cliApp := &fakeApplication{}
	cli, err := NewLocalSocket(cliApp, nil)
	if err != nil {
		t.Fatal(err)
	}
	cliRequest := httptest.NewRequest(http.MethodPost, "http://eri.local/api/v1/messages", strings.NewReader(`{"text":"hello","channel":"conversation_web"}`))
	cliRequest.Header.Set("Content-Type", "application/json")
	cliRecorder := httptest.NewRecorder()
	cli.Handler().ServeHTTP(cliRecorder, cliRequest)
	if cliRecorder.Code != http.StatusBadRequest || len(cliApp.sourceChannels) != 0 {
		t.Fatalf("status=%d source channels=%v", cliRecorder.Code, cliApp.sourceChannels)
	}
}

func TestMultipartMessageRejectsCallerSuppliedChannel(t *testing.T) {
	t.Parallel()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("text", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("channel", "trusted_future_channel"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://eri.local/api/v1/messages", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())

	app := &fakeApplication{}
	server, err := NewLocalSocket(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || len(app.sourceChannels) != 0 {
		t.Fatalf("status=%d source channels=%v", recorder.Code, app.sourceChannels)
	}
}

func TestMultipartMessageAcceptsServerAssignedChannel(t *testing.T) {
	t.Parallel()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("text", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://eri.local/api/v1/messages", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())

	app := &fakeApplication{}
	server, err := NewLocalSocket(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || len(app.sourceChannels) != 1 || app.sourceChannels[0] != "cli" {
		t.Fatalf("status=%d source channels=%v body=%s", recorder.Code, app.sourceChannels, recorder.Body.String())
	}
}

func TestLocalAPIRejectsUnknownAndTrailingJSONFields(t *testing.T) {
	t.Parallel()
	server, err := NewLocalSocket(&fakeApplication{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"text":"hello","unexpected":true}`, `{"text":"hello"}{"text":"second"}`} {
		request := httptest.NewRequest(http.MethodPost, "http://eri.local/api/v1/messages", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestInboundAcceptanceLogContainsCorrelationButNoMessageBody(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	server, err := NewLocalSocket(&fakeApplication{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	privateText := "private-message-body-must-not-be-logged"
	request := httptest.NewRequest(http.MethodPost, "http://eri.local/api/v1/messages", strings.NewReader(`{"text":"`+privateText+`"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	logged := logs.String()
	for _, required := range []string{"inbound message accepted", `"interaction_id":"interaction"`, `"task_id":"task"`, `"source_channel":"cli"`, `"text_bytes":`} {
		if !strings.Contains(logged, required) {
			t.Fatalf("log missing %q: %s", required, logged)
		}
	}
	if strings.Contains(logged, privateText) {
		t.Fatalf("log exposed message body: %s", logged)
	}
}

func TestObservatoryCancellationUsesProtectedCommandBoundary(t *testing.T) {
	app := &fakeApplication{}
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	server, err := NewObservatory(app, assets, nil)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7781/", nil)
	bootstrap.Host = "127.0.0.1:7781"
	bootstrapRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(bootstrapRecorder, bootstrap)
	cookie := bootstrapRecorder.Result().Cookies()[0]
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7781/api/v1/tasks/task-1/cancel", nil)
	request.Host = "127.0.0.1:7781"
	request.AddCookie(cookie)
	request.Header.Set("X-Eri-CSRF", "1")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || app.canceled != "task-1" || !strings.Contains(recorder.Body.String(), "cancel_requested") {
		t.Fatalf("status=%d canceled=%q body=%s", recorder.Code, app.canceled, recorder.Body.String())
	}
}

func TestObservatoryRetryUsesProtectedCommandBoundary(t *testing.T) {
	app := &fakeApplication{}
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	server, err := NewObservatory(app, assets, nil)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7781/", nil)
	bootstrap.Host = "127.0.0.1:7781"
	bootstrapRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(bootstrapRecorder, bootstrap)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7781/api/v1/tasks/failed-task/retry", nil)
	request.Host = "127.0.0.1:7781"
	request.AddCookie(bootstrapRecorder.Result().Cookies()[0])
	request.Header.Set("X-Eri-CSRF", "1")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || app.retried != "failed-task" || !strings.Contains(recorder.Body.String(), "retry-task") {
		t.Fatalf("status=%d retried=%q body=%s", recorder.Code, app.retried, recorder.Body.String())
	}
}

func TestConversationAndObservatorySessionsAreIndependent(t *testing.T) {
	t.Parallel()
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	conversation, _ := NewConversation(&fakeApplication{}, assets, nil)
	observatory, _ := NewObservatory(&fakeApplication{}, assets, nil)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7780/", nil)
	request.Host = "127.0.0.1:7780"
	recorder := httptest.NewRecorder()
	conversation.Handler().ServeHTTP(recorder, request)
	cookie := recorder.Result().Cookies()[0]

	overview := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7781/api/v1/system/overview", nil)
	overview.Host = "127.0.0.1:7781"
	overview.AddCookie(cookie)
	overviewRecorder := httptest.NewRecorder()
	observatory.Handler().ServeHTTP(overviewRecorder, overview)
	if overviewRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("conversation cookie authorized observatory: %d", overviewRecorder.Code)
	}
}

func TestSystemOverviewExposesLayeredDirectedTopology(t *testing.T) {
	t.Parallel()
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	server, err := NewObservatory(&fakeApplication{}, assets, nil)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7781/", nil)
	bootstrap.Host = "127.0.0.1:7781"
	bootstrapRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(bootstrapRecorder, bootstrap)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7781/api/v1/system/overview", nil)
	request.Host = "127.0.0.1:7781"
	request.AddCookie(bootstrapRecorder.Result().Cookies()[0])
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, required := range []string{`"direction":"left_to_right"`, `"from":"channel","to":"runtime"`, `"stage":`, `"lane":`} {
		if recorder.Code != http.StatusOK || !strings.Contains(body, required) {
			t.Fatalf("status=%d missing=%s body=%s", recorder.Code, required, body)
		}
	}
}

func TestConversationExposesOnlyUserSafeRunProjection(t *testing.T) {
	t.Parallel()
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	conversation, err := NewConversation(&fakeApplication{}, assets, nil)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7780/", nil)
	bootstrap.Host = "127.0.0.1:7780"
	bootstrapRecorder := httptest.NewRecorder()
	conversation.Handler().ServeHTTP(bootstrapRecorder, bootstrap)
	cookie := bootstrapRecorder.Result().Cookies()[0]

	for _, target := range []string{"/api/v1/activity", "/api/v1/traces/task"} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7780"+target, nil)
		request.Host = "127.0.0.1:7780"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		conversation.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("target=%s status=%d body=%s", target, recorder.Code, recorder.Body.String())
		}
		body := strings.ToLower(recorder.Body.String())
		for _, forbidden := range []string{"chain_of_thought", "private_prompt", "context_manifest", "tool_result"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("target=%s exposed forbidden field %q: %s", target, forbidden, body)
			}
		}
	}

	for _, target := range []string{
		"/api/v1/system/overview",
		"/api/v1/protocols/ag-ui/events?thread_id=conversation&task_id=task",
		"/api/v1/protocols/a2a/events?context_id=conversation&task_id=task",
	} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7780"+target, nil)
		request.Host = "127.0.0.1:7780"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		conversation.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("conversation exposed developer route %s: %d", target, recorder.Code)
		}
	}

	events := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7780/api/v1/events?once=1", nil)
	events.Host = "127.0.0.1:7780"
	events.AddCookie(cookie)
	eventsRecorder := httptest.NewRecorder()
	conversation.Handler().ServeHTTP(eventsRecorder, events)
	eventsBody := eventsRecorder.Body.String()
	for _, forbidden := range []string{"private_detail", "developer-only", "event-private", "task-private", "aggregate_type", "payload"} {
		if strings.Contains(eventsBody, forbidden) {
			t.Fatalf("conversation event stream exposed %q: %s", forbidden, eventsBody)
		}
	}
}

func TestObservatoryProjectsOnlyTaskScopedProtocolEvents(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 19, 1, 2, 3, 0, time.UTC)
	facts := []eventlog.Event{
		{ID: "started", AggregateType: "task", AggregateID: "task-1", Type: "task.started", Data: map[string]any{"run_id": "run-1"}, Time: now},
		{ID: "tool", AggregateType: "effect", AggregateID: "intent-1", Type: "effect.planned", Data: map[string]any{"task_id": "task-1", "tool_id": "calendar.create", "tool_call_id": "call-1"}, Time: now},
		{ID: "other", AggregateType: "task", AggregateID: "task-2", Type: "task.started", Data: map[string]any{"run_id": "run-2"}, Time: now},
	}
	for i := range facts {
		eventlog.Normalize(&facts[i])
	}
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	server, err := NewObservatory(&fakeApplication{events: facts}, assets, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie := bootstrapCookie(t, server, "127.0.0.1:7781")

	aguiRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7781/api/v1/protocols/ag-ui/events?thread_id=thread-1&task_id=task-1&run_id=run-1", nil)
	aguiRequest.Host = "127.0.0.1:7781"
	aguiRequest.AddCookie(cookie)
	aguiRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(aguiRecorder, aguiRequest)
	aguiBody := aguiRecorder.Body.String()
	for _, required := range []string{`"type":"RUN_STARTED"`, `"threadId":"thread-1"`, `"runId":"run-1"`, `"type":"TOOL_CALL_START"`, `"toolCallId":"call-1"`} {
		if aguiRecorder.Code != http.StatusOK || !strings.Contains(aguiBody, required) {
			t.Fatalf("AG-UI status=%d missing=%s body=%s", aguiRecorder.Code, required, aguiBody)
		}
	}
	if strings.Contains(aguiBody, "run-2") {
		t.Fatalf("AG-UI projection crossed task boundary: %s", aguiBody)
	}

	a2aRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7781/api/v1/protocols/a2a/events?context_id=context-1&task_id=task-1", nil)
	a2aRequest.Host = "127.0.0.1:7781"
	a2aRequest.AddCookie(cookie)
	a2aRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(a2aRecorder, a2aRequest)
	a2aBody := a2aRecorder.Body.String()
	for _, required := range []string{`"artifact_projection":false`, `"statusUpdate"`, `"taskId":"task-1"`, `"contextId":"context-1"`, `"state":"TASK_STATE_WORKING"`} {
		if a2aRecorder.Code != http.StatusOK || !strings.Contains(a2aBody, required) {
			t.Fatalf("A2A status=%d missing=%s body=%s", a2aRecorder.Code, required, a2aBody)
		}
	}
	if strings.Contains(a2aBody, "task-2") || strings.Contains(a2aBody, `"kind"`) || strings.Contains(a2aBody, `"final"`) {
		t.Fatalf("A2A projection is unscoped or uses removed v0.3 fields: %s", a2aBody)
	}
}

func TestProtocolPreviewRequiresExplicitScope(t *testing.T) {
	t.Parallel()
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	server, err := NewObservatory(&fakeApplication{}, assets, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie := bootstrapCookie(t, server, "127.0.0.1:7781")
	for _, target := range []string{
		"/api/v1/protocols/ag-ui/events?thread_id=thread-1",
		"/api/v1/protocols/a2a/events?context_id=context-1",
	} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7781"+target, nil)
		request.Host = "127.0.0.1:7781"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("target=%s status=%d body=%s", target, recorder.Code, recorder.Body.String())
		}
	}
}

func TestMemoryInspectorRemainsInsideObservatorySession(t *testing.T) {
	t.Parallel()
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Eri")}}
	conversation, _ := NewConversation(&fakeApplication{}, assets, nil)
	observatory, _ := NewObservatory(&fakeApplication{}, assets, nil)

	conversationCookie := bootstrapCookie(t, conversation, "127.0.0.1:7780")
	conversationRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7780/api/v1/memory", nil)
	conversationRequest.Host = "127.0.0.1:7780"
	conversationRequest.AddCookie(conversationCookie)
	conversationRecorder := httptest.NewRecorder()
	conversation.Handler().ServeHTTP(conversationRecorder, conversationRequest)
	if conversationRecorder.Code != http.StatusNotFound {
		t.Fatalf("conversation memory status=%d", conversationRecorder.Code)
	}

	observatoryCookie := bootstrapCookie(t, observatory, "127.0.0.1:7781")
	observatoryRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7781/api/v1/memory", nil)
	observatoryRequest.Host = "127.0.0.1:7781"
	observatoryRequest.AddCookie(observatoryCookie)
	observatoryRecorder := httptest.NewRecorder()
	observatory.Handler().ServeHTTP(observatoryRecorder, observatoryRequest)
	if observatoryRecorder.Code != http.StatusOK || !strings.Contains(observatoryRecorder.Body.String(), `"stages":["stored"]`) {
		t.Fatalf("observatory status=%d body=%s", observatoryRecorder.Code, observatoryRecorder.Body.String())
	}
}

func bootstrapCookie(t *testing.T, server *Server, host string) *http.Cookie {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	request.Host = host
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder.Result().Cookies()[0]
}

func postMessageRequest() *http.Request {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7780/api/v1/messages", strings.NewReader(`{"text":"hello"}`))
	request.Host = "127.0.0.1:7780"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Eri-CSRF", "1")
	return request
}
