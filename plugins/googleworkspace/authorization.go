package googleworkspace

import (
	"context"
	"fmt"
	"sort"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
)

var capabilityScope = map[string]string{
	"calendar_read":  calendarReadScope,
	"calendar_write": calendarWriteScope,
	"gmail_metadata": gmailMetadataScope,
	"gmail_send":     gmailSendScope,
}

type authorizationInput struct {
	Capabilities []string `json:"capabilities" jsonschema:"one or more of calendar_read, calendar_write, gmail_metadata, gmail_send"`
}

type authorizationStatusOutput struct {
	Authorized       bool     `json:"authorized"`
	Capabilities     []string `json:"capabilities"`
	Missing          []string `json:"missing,omitempty"`
	CredentialSource string   `json:"credential_source,omitempty"`
}

type authorizationStartOutput struct {
	AuthorizationURL string    `json:"authorization_url"`
	ExpiresAt        time.Time `json:"expires_at"`
	Capabilities     []string  `json:"capabilities"`
}

type authorizationRevokeOutput struct {
	Revoked   bool      `json:"revoked"`
	RevokedAt time.Time `json:"revoked_at"`
	Receipt   string    `json:"receipt"`
}

func (s *Server) authorizationStatus(ctx context.Context, _ *protocol.CallToolRequest, input authorizationInput) (*protocol.CallToolResult, authorizationStatusOutput, error) {
	scopes, capabilities, err := requestedCapabilityScopes(input.Capabilities)
	if err != nil {
		return nil, authorizationStatusOutput{}, err
	}
	status, err := s.broker.status(ctx, scopes)
	if err != nil {
		return nil, authorizationStatusOutput{}, err
	}
	missing := capabilitiesForScopes(status.MissingScopes)
	return nil, authorizationStatusOutput{
		Authorized: status.Authorized, Capabilities: capabilities, Missing: missing,
		CredentialSource: status.CredentialSource,
	}, nil
}

func (s *Server) authorizationStart(ctx context.Context, _ *protocol.CallToolRequest, input authorizationInput) (*protocol.CallToolResult, authorizationStartOutput, error) {
	scopes, capabilities, err := requestedCapabilityScopes(input.Capabilities)
	if err != nil {
		return nil, authorizationStartOutput{}, err
	}
	started, err := s.broker.start(ctx, scopes)
	if err != nil {
		return nil, authorizationStartOutput{}, err
	}
	return nil, authorizationStartOutput{
		AuthorizationURL: started.AuthorizationURL, ExpiresAt: started.ExpiresAt, Capabilities: capabilities,
	}, nil
}

func (s *Server) authorizationRevoke(ctx context.Context, _ *protocol.CallToolRequest, _ struct{}) (*protocol.CallToolResult, authorizationRevokeOutput, error) {
	revoked, err := s.broker.revoke(ctx)
	if err != nil {
		return nil, authorizationRevokeOutput{}, err
	}
	return providerResult(revoked.Receipt, ""), authorizationRevokeOutput{
		Revoked: revoked.Revoked, RevokedAt: revoked.RevokedAt, Receipt: revoked.Receipt,
	}, nil
}

func requestedCapabilityScopes(capabilities []string) ([]string, []string, error) {
	if len(capabilities) == 0 || len(capabilities) > len(capabilityScope) {
		return nil, nil, fmt.Errorf("one or more Google capabilities are required")
	}
	seen := make(map[string]struct{}, len(capabilities))
	resultCapabilities := make([]string, 0, len(capabilities))
	scopes := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		scope, ok := capabilityScope[capability]
		if !ok {
			return nil, nil, fmt.Errorf("unknown Google capability %q", capability)
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
		resultCapabilities = append(resultCapabilities, capability)
		scopes = append(scopes, scope)
	}
	sort.Strings(resultCapabilities)
	sort.Strings(scopes)
	return scopes, resultCapabilities, nil
}

func capabilitiesForScopes(scopes []string) []string {
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		for capability, candidate := range capabilityScope {
			if candidate == scope {
				result = append(result, capability)
				break
			}
		}
	}
	sort.Strings(result)
	return result
}
