// Package daemon is Eri's sole composition root and process lifecycle owner.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/z-chenhao/eri/internal/agent"
	"github.com/z-chenhao/eri/internal/approval"
	"github.com/z-chenhao/eri/internal/channel"
	larkchannel "github.com/z-chenhao/eri/internal/channel/lark"
	"github.com/z-chenhao/eri/internal/codex"
	"github.com/z-chenhao/eri/internal/config"
	"github.com/z-chenhao/eri/internal/content"
	"github.com/z-chenhao/eri/internal/delivery"
	"github.com/z-chenhao/eri/internal/episode"
	"github.com/z-chenhao/eri/internal/evolution"
	"github.com/z-chenhao/eri/internal/feedback"
	"github.com/z-chenhao/eri/internal/identifier"
	"github.com/z-chenhao/eri/internal/identity"
	"github.com/z-chenhao/eri/internal/localapi"
	"github.com/z-chenhao/eri/internal/memory"
	"github.com/z-chenhao/eri/internal/model/deepseek"
	"github.com/z-chenhao/eri/internal/model/ollama"
	"github.com/z-chenhao/eri/internal/notification"
	"github.com/z-chenhao/eri/internal/observability"
	"github.com/z-chenhao/eri/internal/plugin"
	pluginmcp "github.com/z-chenhao/eri/internal/plugin/mcp"
	"github.com/z-chenhao/eri/internal/runtime"
	"github.com/z-chenhao/eri/internal/scheduler"
	"github.com/z-chenhao/eri/internal/skill"
	"github.com/z-chenhao/eri/internal/store/sqlite"
	"github.com/z-chenhao/eri/internal/subagent"
	assistanttask "github.com/z-chenhao/eri/internal/task"
	"github.com/z-chenhao/eri/internal/tool"
	"github.com/z-chenhao/eri/internal/tool/builtin"
	"github.com/z-chenhao/eri/internal/userdata"
	eriskills "github.com/z-chenhao/eri/skills"
	conversationweb "github.com/z-chenhao/eri/web/conversation"
	observatoryweb "github.com/z-chenhao/eri/web/observatory"
)

type Dependencies struct {
	MasterKey       []byte
	Model           agent.Model
	Notifier        notification.Sender
	WebClient       *http.Client
	TavilyEndpoint  string
	AllowPrivateWeb bool
	Judge           agent.Judge
	Logger          *slog.Logger
	CodexRunner     codex.Runner
	LarkPlatform    larkchannel.Platform
}

type application struct {
	*channel.Service
	approvals   *approval.Service
	observation *observability.Service
	episodes    *episode.Service
	plugins     *plugin.Manager
	evolution   *evolution.Service
	tasks       *assistanttask.Service
}

func (a *application) Runs(ctx context.Context, limit int) ([]observability.RunSummary, error) {
	return a.observation.Runs(ctx, limit)
}

func (a *application) Run(ctx context.Context, id string) (observability.RunDetail, bool, error) {
	return a.observation.Run(ctx, id)
}

func (a *application) ConversationActivity(ctx context.Context, limit int) (observability.ConversationActivity, error) {
	return a.observation.ConversationActivity(ctx, limit)
}

func (a *application) ConversationTrace(ctx context.Context, taskID string) (observability.ConversationTrace, bool, error) {
	return a.observation.ConversationTrace(ctx, taskID)
}

func (a *application) MemoryOverview(ctx context.Context, limit int) (observability.MemoryOverview, error) {
	return a.observation.MemoryOverview(ctx, limit)
}

func (a *application) Episodes(ctx context.Context, limit int) ([]episode.Record, error) {
	return a.episodes.List(ctx, limit)
}

func (a *application) Episode(ctx context.Context, id string) (episode.Manifest, bool, error) {
	return a.episodes.Inspect(ctx, id)
}

func (a *application) ExportEpisode(ctx context.Context, id string) (episode.Manifest, bool, error) {
	return a.episodes.Export(ctx, id)
}

func (a *application) PromoteEpisode(ctx context.Context, id string) (episode.DatasetCandidate, error) {
	return a.episodes.Promote(ctx, id)
}

func (a *application) DatasetSnapshots(ctx context.Context, limit int) ([]episode.DatasetSnapshot, error) {
	return a.episodes.DatasetSnapshots(ctx, limit)
}

func (a *application) FreezeDataset(ctx context.Context, purpose, splitSeed string, candidateIDs []string) (episode.DatasetSnapshot, error) {
	return a.episodes.FreezeDataset(ctx, purpose, splitSeed, candidateIDs)
}

func (a *application) DatasetSnapshot(ctx context.Context, id string) (episode.DatasetSnapshotManifest, bool, error) {
	return a.episodes.InspectDatasetSnapshot(ctx, id)
}

func (a *application) Plugins(ctx context.Context) ([]plugin.Record, error) {
	return a.plugins.List(ctx)
}

func (a *application) EvolutionReleases(ctx context.Context, limit int) ([]evolution.Release, error) {
	return a.evolution.Releases(ctx, limit)
}

func (a *application) RollbackEvolution(ctx context.Context, id string) error {
	return a.evolution.Rollback(ctx, id)
}

func (a *application) CancelTask(ctx context.Context, id string) (assistanttask.CancelResult, error) {
	return a.tasks.Cancel(ctx, id)
}

func (a *application) RetryTask(ctx context.Context, id string) (assistanttask.RetryResult, error) {
	return a.tasks.Retry(ctx, id)
}

func (a *application) DecideApproval(ctx context.Context, id string, decision approval.Decision) (approval.Result, error) {
	return a.approvals.Decide(ctx, id, decision)
}

type Daemon struct {
	config         config.Config
	store          *sqlite.Store
	worker         *runtime.Worker
	scheduler      *scheduler.Worker
	approvalExpiry *approval.ExpiryWorker
	lark           *larkchannel.Service
	extensionHost  *pluginmcp.Host
	conversation   *http.Server
	observatory    *http.Server
	local          *http.Server
	logger         *slog.Logger
	logFile        io.Closer
	cancel         context.CancelFunc
	closeOnce      sync.Once
	readyOnce      sync.Once
	ready          chan struct{}
}

func New(ctx context.Context, cfg config.Config, dependencies Dependencies) (_ *Daemon, resultErr error) {
	if err := PrepareDataRoot(cfg.DataRoot); err != nil {
		return nil, err
	}
	logger := dependencies.Logger
	var logFile io.Closer
	if logger == nil {
		configured, closer, err := observability.NewProcessLogger(filepath.Join(cfg.DataRoot, "logs", "daemon.log"), nil)
		if err != nil {
			return nil, fmt.Errorf("open daemon log: %w", err)
		}
		logFile = closer
		logger = configured
	}
	initialized := false
	defer func() {
		if initialized {
			return
		}
		if resultErr != nil {
			logger.Error("Eri daemon initialization failed", "component", "daemon", "error_code", observability.ErrorCode(resultErr), "error", observability.SafeError(resultErr))
		}
		if logFile != nil {
			_ = logFile.Close()
		}
	}()
	key := dependencies.MasterKey
	if len(key) == 0 {
		var err error
		key, err = content.LoadOrCreateMasterKey(ctx, cfg.DataRoot)
		if err != nil {
			return nil, err
		}
	}
	contentStore, err := content.New(filepath.Join(cfg.DataRoot, "content"), key)
	if err != nil {
		return nil, err
	}
	store, err := sqlite.Open(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	model := dependencies.Model
	if model == nil {
		switch cfg.ModelProvider {
		case "", "ollama":
			model = ollama.New(cfg.OllamaURL, cfg.Model, cfg.ModelTimeout, logger, cfg.DebugLog)
		case "deepseek":
			if cfg.DeepSeekKeySet {
				model, err = deepseek.New(cfg.DeepSeekURL, os.Getenv("DEEPSEEK_API_KEY"), cfg.Model, cfg.ModelTimeout, logger, cfg.DebugLog)
			} else {
				model, err = deepseek.NewViaBroker(cfg.ProviderBrokerSocket, cfg.Model, cfg.ModelTimeout, logger, cfg.DebugLog)
			}
			if err != nil {
				store.Close()
				return nil, err
			}
		default:
			store.Close()
			return nil, fmt.Errorf("unsupported model provider %q", cfg.ModelProvider)
		}
	}
	owner := identifier.MustNew()
	conversationService := channel.NewService(store, contentStore)
	applicationService := &application{Service: conversationService, approvals: approval.NewService(store)}
	var larkService *larkchannel.Service
	if cfg.LarkEnabled {
		ownerOpenID := cfg.LarkOwnerOpenID
		if ownerOpenID == "" {
			ownerOpenID, err = store.UniqueExternalSender(ctx, "lark")
			if err != nil {
				store.Close()
				return nil, fmt.Errorf("recover Lark owner binding: %w", err)
			}
			if ownerOpenID == "" {
				store.Close()
				return nil, fmt.Errorf("LARK_ERI_OWNER_OPEN_ID is required until an existing unique owner binding can be recovered")
			}
		}
		platform := dependencies.LarkPlatform
		if platform == nil {
			platform, err = larkchannel.NewSDKPlatform(cfg.LarkAppID, os.Getenv("LARK_ERI_API_SECRET"), cfg.LarkBrand)
			if err != nil {
				store.Close()
				return nil, fmt.Errorf("initialize Lark platform: %w", err)
			}
		}
		larkService, err = larkchannel.NewService(ownerOpenID, conversationService, platform, logger)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("initialize Lark channel: %w", err)
		}
	}
	workspaceRoot := cfg.WorkspaceRoot
	if workspaceRoot == "" {
		workspaceRoot, err = os.Getwd()
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("resolve tool workspace: %w", err)
		}
	}
	filesTool, err := builtin.NewFiles(workspaceRoot)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize file built-in: %w", err)
	}
	var webTool tool.Tool
	if cfg.TavilyKeySet {
		webTool, err = builtin.NewWeb(
			dependencies.WebClient, dependencies.TavilyEndpoint, os.Getenv("TAVILY_API_KEY"),
			cfg.TavilySearchDepth, cfg.TavilyExtractDepth, dependencies.AllowPrivateWeb,
		)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("initialize web built-in: %w", err)
		}
	}
	terminalTool, err := builtin.NewTerminal(workspaceRoot)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize terminal built-in: %w", err)
	}
	var semanticEncoder memory.SemanticEncoder
	if dependencies.Model == nil {
		embeddingModel, discoveryErr := ollama.DiscoverEmbeddingModel(ctx, cfg.OllamaURL, cfg.MemoryEmbeddingModel)
		if discoveryErr != nil {
			logger.Info("local semantic memory is unavailable", "component", "memory", "operation", "embedding_discovery", "semantic_status", "disabled", "error_code", observability.ErrorCode(discoveryErr))
		} else if embeddingModel == "" {
			logger.Info("local semantic memory is unavailable", "component", "memory", "operation", "embedding_discovery", "semantic_status", "disabled", "reason", "no_local_embedding_model")
		} else {
			embedder, embedErr := ollama.NewEmbedder(cfg.OllamaURL, embeddingModel, 2*time.Minute)
			if embedErr != nil {
				logger.Warn("local semantic memory configuration failed", "component", "memory", "operation", "embedding_configuration", "error_code", observability.ErrorCode(embedErr), "error", observability.SafeError(embedErr))
			} else {
				semanticEncoder = embedder
				logger.Info("local semantic memory configured", "component", "memory", "operation", "embedding_configuration", "model", embedder.ID())
			}
		}
	}
	memoryOptions := memory.Options{Logger: logger}
	if semanticEncoder != nil {
		memoryOptions.SemanticEncoder = semanticEncoder
		memoryOptions.SemanticIndex = store
	}
	memoryService, err := memory.NewService(store, contentStore, key, memoryOptions)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize memory service: %w", err)
	}
	if err := memoryService.RecoverDeletes(ctx); err != nil {
		store.Close()
		return nil, fmt.Errorf("recover memory deletions: %w", err)
	}
	if _, err := memoryService.Consolidate(ctx); err != nil {
		store.Close()
		return nil, fmt.Errorf("consolidate memory: %w", err)
	}
	memoryTool, err := builtin.NewMemory(memoryService, contentStore)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize memory built-in: %w", err)
	}
	feedbackService, err := feedback.NewService(store, contentStore)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize feedback service: %w", err)
	}
	feedbackTool, err := builtin.NewFeedback(feedbackService)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize feedback built-in: %w", err)
	}
	userDataService := userdata.NewService(store, contentStore)
	userDataTool, err := builtin.NewUserData(userDataService, contentStore)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize user data built-in: %w", err)
	}
	commitmentService := scheduler.NewService(store, contentStore)
	commitmentTool, err := builtin.NewScheduler(commitmentService)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize commitment built-in: %w", err)
	}
	taskService := assistanttask.NewService(store, contentStore)
	applicationService.tasks = taskService
	taskTool, err := builtin.NewTasks(taskService)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize task built-in: %w", err)
	}
	notifier := dependencies.Notifier
	if notifier == nil {
		notifier = notification.LocalSender{}
	}
	notificationTool, err := builtin.NewNotification(notifier)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize notification built-in: %w", err)
	}
	mcpSpecs, err := pluginmcp.ParseSpecs(cfg.MCPServersJSON)
	if err != nil {
		store.Close()
		return nil, err
	}
	extensionHost, err := pluginmcp.OpenHost(ctx, mcpSpecs, logger)
	if err != nil {
		store.Close()
		return nil, err
	}
	pluginManager, err := plugin.NewManager(filepath.Join(cfg.DataRoot, "plugins"), workspaceRoot, extensionHost)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("initialize plugin manager: %w", err)
	}
	userDataService.AddSupplement(pluginManager)
	if err := pluginManager.LoadInstalled(ctx); err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("load installed plugins: %w", err)
	}
	pluginTool, err := builtin.NewPlugins(pluginManager)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("initialize plugin built-in: %w", err)
	}
	skillCatalog, err := skill.Open(eriskills.Builtin, skill.Options{UserSkillRoot: cfg.UserSkillRoot})
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("load Agent Skills: %w", err)
	}
	for _, diagnostic := range skillCatalog.Diagnostics() {
		logger.Warn("Agent Skill diagnostic", "component", "skill", "location", observability.SafeText(diagnostic.Location, 240), "message", observability.SafeText(diagnostic.Message, 500))
	}
	skillTool, err := builtin.NewSkills(ctx, skillCatalog)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("initialize Agent Skills built-in: %w", err)
	}
	availableTools := []tool.Tool{filesTool, terminalTool}
	if webTool != nil {
		availableTools = append(availableTools, webTool)
	}
	availableTools = append(availableTools, memoryTool, feedbackTool, userDataTool, taskTool, commitmentTool, notificationTool, skillTool, pluginTool)
	availableTools = append(availableTools, extensionHost.Tools()...)
	toolGateway, err := tool.NewGateway(store, contentStore, availableTools...)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("initialize tool gateway: %w", err)
	}
	if err := pluginManager.BindGateway(toolGateway); err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("bind plugin gateway: %w", err)
	}
	applicationService.plugins = pluginManager
	// Provider usage remains observable, but Eri does not impose a separate
	// per-task, daily, or monthly token ceiling. Context capacity, deadlines,
	// no-progress recovery, policy and provider-side account limits remain the
	// governing resource boundaries.
	var modelBudget agent.ModelBudget
	nativeSubagent, err := agent.NewNativeSubagent(
		store, contentStore, model, toolGateway, modelBudget, cfg.MaxOutputTokens,
		cfg.ModelProvider == "deepseek", modelTarget(cfg, dependencies.Model != nil), logger,
	)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("initialize native subagent: %w", err)
	}
	providers := []subagent.Provider{nativeSubagent}
	var codexService *codex.Service
	codexRunner := dependencies.CodexRunner
	if codexRunner == nil {
		executable, found, err := codex.DiscoverExecutable(cfg.CodexPath)
		if err != nil {
			extensionHost.Close()
			store.Close()
			return nil, fmt.Errorf("discover local Codex: %w", err)
		}
		if found {
			codexRunner, err = codex.NewLocalRunner(executable, cfg.DataRoot, cfg.CodexTimeout)
			if err != nil {
				extensionHost.Close()
				store.Close()
				return nil, fmt.Errorf("initialize local Codex runner: %w", err)
			}
		}
	}
	if codexRunner != nil {
		codexService, err = codex.NewService(store, contentStore, codexRunner, workspaceRoot)
		if err != nil {
			extensionHost.Close()
			store.Close()
			return nil, fmt.Errorf("initialize Codex delegation service: %w", err)
		}
		providers = append(providers, codexService)
		userDataService.AddSupplement(codexService)
	}
	bindings := []subagent.Binding{{RoleID: "intern", ProviderID: "eri_native"}}
	if codexService != nil {
		bindings = append(bindings, subagent.Binding{RoleID: "engineering_team", ProviderID: "codex"})
	}
	subagentRegistry, err := subagent.NewRegistry(subagent.DefaultRoles(), bindings, providers...)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("initialize subagent registry: %w", err)
	}
	delegateTool, err := builtin.NewDelegate(subagentRegistry)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("initialize delegation built-in: %w", err)
	}
	if err := toolGateway.RegisterBuiltIn(delegateTool); err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("register delegation built-in: %w", err)
	}
	evolutionService, err := evolution.NewService(store, contentStore, model, modelBudget, logger)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, fmt.Errorf("initialize evolution service: %w", err)
	}
	applicationService.evolution = evolutionService
	agentService := agent.NewService(store, contentStore, model, identity.Default(), owner, toolGateway, memoryService, agent.LoopConfig{
		MaxEvalAttempts: cfg.MaxEvalAttempts, MaxOutputTokens: cfg.MaxOutputTokens,
		ApprovalTTL:   cfg.ApprovalTTL,
		ExternalModel: cfg.ModelProvider == "deepseek", Budget: modelBudget, Skills: skillCatalog, Judge: dependencies.Judge,
		ModelTarget: modelTarget(cfg, dependencies.Model != nil), Evolution: evolutionService, Logger: logger,
	})
	deliveryAdapters := make([]delivery.Adapter, 0, 1)
	if larkService != nil {
		deliveryAdapters = append(deliveryAdapters, larkService)
	}
	deliveryService := delivery.NewService(store, contentStore, deliveryAdapters...)
	episodeService := episode.NewService(store, contentStore)
	applicationService.observation = observability.NewService(store, contentStore)
	applicationService.episodes = episodeService
	handlers := map[string]runtime.Handler{
		"task.wake":       agentService.HandleWake,
		"approval.resume": agentService.HandleApprovalResume,
		"subagent.resume": agentService.HandleSubagentResume,
		"effect.reconcile": func(ctx context.Context, item runtime.OutboxItem) error {
			return toolGateway.Reconcile(ctx, item.AggregateID, item.Attempts)
		},
		"delivery.send":      deliveryService.HandleSend,
		"episode.build":      episodeService.HandleBuild,
		"evolution.feedback": evolutionService.HandleFeedback,
		"evolution.propose":  evolutionService.HandlePropose,
		"data.erase":         userDataService.HandleErase,
	}
	handlers["subagent.eri_native.run"] = nativeSubagent.HandleRun
	if codexService != nil {
		handlers["subagent.codex.run"] = codexService.HandleRun
	}
	worker := runtime.NewWorker(store, handlers, owner, cfg.PollInterval, logger)
	schedulerWorker := scheduler.NewWorker(store, cfg.PollInterval, logger)
	approvalExpiryWorker := approval.NewExpiryWorker(store, cfg.PollInterval, logger)

	conversationAPI, err := localapi.NewConversation(applicationService, conversationweb.Assets, logger)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, err
	}
	observatoryAPI, err := localapi.NewObservatory(applicationService, observatoryweb.Assets, logger)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, err
	}
	localAPI, err := localapi.NewLocalSocket(applicationService, logger)
	if err != nil {
		extensionHost.Close()
		store.Close()
		return nil, err
	}

	d := &Daemon{
		config: cfg, store: store, worker: worker, scheduler: schedulerWorker, approvalExpiry: approvalExpiryWorker, lark: larkService, extensionHost: extensionHost, logger: logger, logFile: logFile,
		conversation: newHTTPServer(conversationAPI.Handler()),
		observatory:  newHTTPServer(observatoryAPI.Handler()),
		ready:        make(chan struct{}),
	}
	localAPI.SetStopHandler(d.Stop)
	d.local = newHTTPServer(localAPI.Handler())
	initialized = true
	return d, nil
}

func modelTarget(cfg config.Config, injected bool) string {
	provider := cfg.ModelProvider
	if provider == "" {
		provider = "ollama"
	}
	if injected {
		provider = "injected"
	}
	model := cfg.Model
	if model == "" {
		return provider
	}
	return provider + ":" + model
}

func (d *Daemon) Run(parent context.Context) (runErr error) {
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	defer func() {
		cancel()
		if runErr != nil {
			d.logger.Error("Eri daemon run failed", "component", "daemon", "error_code", observability.ErrorCode(runErr), "error", observability.SafeError(runErr))
		}
		d.shutdown()
	}()
	if err := validateLoopbackAddress(d.config.ConversationAddr); err != nil {
		return fmt.Errorf("conversation address: %w", err)
	}
	if err := validateLoopbackAddress(d.config.ObservatoryAddr); err != nil {
		return fmt.Errorf("observatory address: %w", err)
	}
	conversationListener, err := net.Listen("tcp", d.config.ConversationAddr)
	if err != nil {
		return fmt.Errorf("listen for conversation web: %w", err)
	}
	defer conversationListener.Close()
	observatoryListener, err := net.Listen("tcp", d.config.ObservatoryAddr)
	if err != nil {
		return fmt.Errorf("listen for observatory: %w", err)
	}
	defer observatoryListener.Close()
	localListener, err := listenUnix(d.config.SocketPath)
	if err != nil {
		return err
	}
	defer func() {
		localListener.Close()
		os.Remove(d.config.SocketPath)
	}()
	recovery, err := d.store.RecoverRuntime(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("recover durable runtime: %w", err)
	}
	d.logger.Info("durable runtime recovery completed", "component", "runtime", "running_tasks", recovery.RunningTasks, "outbox_items", recovery.OutboxItems, "ambiguous_effects", recovery.AmbiguousEffects)

	errCh := make(chan error, 7)
	go func() { errCh <- d.worker.Run(ctx) }()
	go func() { errCh <- d.scheduler.Run(ctx) }()
	go func() { errCh <- d.approvalExpiry.Run(ctx) }()
	go serveHTTP(d.conversation, conversationListener, errCh)
	go serveHTTP(d.observatory, observatoryListener, errCh)
	go serveHTTP(d.local, localListener, errCh)
	if d.lark != nil {
		larkErr := make(chan error, 1)
		go func() { larkErr <- d.lark.Run(ctx) }()
		select {
		case <-d.lark.Ready():
			go func() { errCh <- <-larkErr }()
		case err := <-larkErr:
			if err == nil {
				err = fmt.Errorf("Lark channel stopped before becoming ready")
			}
			return fmt.Errorf("start Lark channel: %w", err)
		case <-ctx.Done():
			return nil
		}
	}
	d.logger.Info("Eri daemon started", "component", "daemon", "conversation", "http://"+d.config.ConversationAddr, "observatory", "http://"+d.config.ObservatoryAddr, "socket", observability.SafeText(d.config.SocketPath, 240))
	d.readyOnce.Do(func() { close(d.ready) })

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			cancel()
			return err
		}
	}
	return nil
}

// Ready closes only after all listeners are bound, durable recovery succeeds,
// and the daemon workers and HTTP servers have been started.
func (d *Daemon) Ready() <-chan struct{} { return d.ready }

func (d *Daemon) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *Daemon) Close() { d.shutdown() }

func (d *Daemon) shutdown() {
	d.closeOnce.Do(func() {
		d.logger.Info("Eri daemon stopping", "component", "daemon")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, server := range []*http.Server{d.conversation, d.observatory, d.local} {
			if server != nil {
				if err := server.Shutdown(ctx); err != nil {
					d.logger.Error("HTTP server shutdown failed", "component", "daemon", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
				}
			}
		}
		if d.lark != nil {
			if err := d.lark.Stop(ctx); err != nil {
				d.logger.Error("Lark channel shutdown failed", "component", "lark_channel", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
			}
		}
		if d.extensionHost != nil {
			if err := d.extensionHost.Close(); err != nil {
				d.logger.Error("plugin host shutdown failed", "component", "daemon", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
			}
		}
		if d.store != nil {
			if err := d.store.Close(); err != nil {
				d.logger.Error("store shutdown failed", "component", "daemon", "error_code", observability.ErrorCode(err), "error", observability.SafeError(err))
			}
		}
		d.logger.Info("Eri daemon stopped", "component", "daemon")
		if d.logFile != nil {
			_ = d.logFile.Close()
		}
	})
}

// PrepareDataRoot establishes the private filesystem boundary needed by both
// first-run bootstrap and the fully composed daemon.
func PrepareDataRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return fmt.Errorf("Eri data root must be an absolute non-root directory")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create Eri data root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve Eri data root: %w", err)
	}
	if filepath.Clean(resolved) == string(filepath.Separator) {
		return fmt.Errorf("Eri data root must not resolve to the filesystem root")
	}
	if err := os.Chmod(resolved, 0o700); err != nil {
		return fmt.Errorf("protect Eri data root: %w", err)
	}
	for _, directory := range []string{"configuration", "metadata", "content", "indexes", "plugins", "episodes", "datasets", "backups", "exports", "runtime", "logs"} {
		path := filepath.Join(resolved, directory)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create Eri data directory %s: %w", directory, err)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("Eri data directory %s must be a real directory inside EriDataRoot", directory)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("protect Eri data directory %s: %w", directory, err)
		}
	}
	return nil
}

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("must bind a numeric loopback address, got %q", host)
	}
	return nil
}

func listenUnix(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket path %s", path)
		}
		connection, dialErr := net.DialTimeout("unix", path, 150*time.Millisecond)
		if dialErr == nil {
			connection.Close()
			return nil, fmt.Errorf("Eri daemon is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale daemon socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect daemon socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on daemon socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("protect daemon socket: %w", err)
	}
	return listener, nil
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
}

func serveHTTP(server *http.Server, listener net.Listener, errors chan<- error) {
	err := server.Serve(listener)
	if errorsIsServerClosed(err) {
		errors <- nil
		return
	}
	errors <- err
}

func errorsIsServerClosed(err error) bool { return errors.Is(err, http.ErrServerClosed) }
