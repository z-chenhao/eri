// Package googleworkspace implements the official Eri Google Workspace MCP
// plugin. It receives one-use capability handles from Eri and redeems them
// directly with an external Auth Broker; provider tokens never enter Eri Core.
package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
)

const pluginID = "google-workspace"

var allScopes = []string{calendarReadScope, calendarWriteScope, gmailMetadataScope, gmailSendScope}

type Options struct {
	BrokerEndpoint string
	Provider       string
	AllowedScopes  []string
	HTTPClient     *http.Client
	CalendarBase   string
	GmailBase      string
}

type Server struct {
	broker *brokerClient
	google *googleClient
}

func New(options Options) (*Server, error) {
	if options.Provider == "" {
		options.Provider = "google"
	}
	if len(options.AllowedScopes) == 0 {
		options.AllowedScopes = append([]string(nil), allScopes...)
	}
	if options.CalendarBase == "" {
		options.CalendarBase = "https://www.googleapis.com"
	}
	if options.GmailBase == "" {
		options.GmailBase = "https://gmail.googleapis.com"
	}
	broker, err := newBrokerClient(options.BrokerEndpoint, options.Provider, pluginID, options.AllowedScopes)
	if err != nil {
		return nil, err
	}
	google, err := newGoogleClient(options.HTTPClient, options.CalendarBase, options.GmailBase)
	if err != nil {
		return nil, err
	}
	return &Server{broker: broker, google: google}, nil
}

func FromEnvironment() (*Server, error) {
	var scopes []string
	if err := json.Unmarshal([]byte(os.Getenv("ERI_AUTH_SCOPES_JSON")), &scopes); err != nil {
		return nil, fmt.Errorf("decode ERI_AUTH_SCOPES_JSON: %w", err)
	}
	return New(Options{
		BrokerEndpoint: strings.TrimSpace(os.Getenv("ERI_AUTH_BROKER_ENDPOINT")),
		Provider:       strings.TrimSpace(os.Getenv("ERI_AUTH_PROVIDER")), AllowedScopes: scopes,
	})
}

func (s *Server) MCP() *protocol.Server {
	server := protocol.NewServer(&protocol.Implementation{Name: "eri-google-workspace", Version: "0.1.0"}, nil)
	protocol.AddTool(server, &protocol.Tool{Name: "authorization_status", Description: "Check whether the external Google authorization broker has the requested capability; this never returns a credential."}, s.authorizationStatus)
	protocol.AddTool(server, &protocol.Tool{Name: "authorization_start", Description: "Create a short-lived Google consent URL for the requested capabilities; the external broker, not Eri Core, receives and stores the offline grant."}, s.authorizationStart)
	protocol.AddTool(server, &protocol.Tool{Name: "authorization_disconnect", Description: "Revoke the Google offline grant and delete it from the external OS credential store."}, s.authorizationRevoke)
	protocol.AddTool(server, &protocol.Tool{Name: "calendar_list_events", Description: "List verified Google Calendar events in a bounded RFC3339 time window."}, s.calendarList)
	protocol.AddTool(server, &protocol.Tool{Name: "calendar_create_event", Description: "Create one Google Calendar event and return its provider event ID and receipt."}, s.calendarCreate)
	protocol.AddTool(server, &protocol.Tool{Name: "gmail_list_message_metadata", Description: "List bounded Gmail message IDs and thread IDs without reading message bodies."}, s.gmailList)
	protocol.AddTool(server, &protocol.Tool{Name: "gmail_get_message_metadata", Description: "Read selected Gmail message headers and snippet without returning the full body."}, s.gmailMetadata)
	protocol.AddTool(server, &protocol.Tool{Name: "gmail_send", Description: "Send one plain-text Gmail message and return the provider message ID and receipt."}, s.gmailSend)
	return server
}

func Run(ctx context.Context) error {
	server, err := FromEnvironment()
	if err != nil {
		return err
	}
	return server.MCP().Run(ctx, &protocol.StdioTransport{})
}
