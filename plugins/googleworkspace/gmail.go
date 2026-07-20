package googleworkspace

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	gmailMetadataScope = "https://www.googleapis.com/auth/gmail.metadata"
	gmailSendScope     = "https://www.googleapis.com/auth/gmail.send"
)

type gmailListInput struct {
	MaxResults int      `json:"max_results,omitempty" jsonschema:"maximum messages from 1 to 100"`
	PageToken  string   `json:"page_token,omitempty" jsonschema:"opaque Gmail page token"`
	LabelIDs   []string `json:"label_ids,omitempty" jsonschema:"labels that every result must have"`
}

type gmailMessageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type gmailListResponse struct {
	Messages           []gmailMessageRef `json:"messages"`
	NextPageToken      string            `json:"nextPageToken,omitempty"`
	ResultSizeEstimate int               `json:"resultSizeEstimate"`
}

type gmailListOutput struct {
	Messages           []gmailMessageRef `json:"messages"`
	NextPageToken      string            `json:"next_page_token,omitempty"`
	ResultSizeEstimate int               `json:"result_size_estimate"`
	Receipt            string            `json:"receipt"`
}

type gmailMetadataInput struct {
	MessageID string   `json:"message_id" jsonschema:"Gmail message id"`
	Headers   []string `json:"headers,omitempty" jsonschema:"requested RFC 2822 header names"`
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailMetadataResponse struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	Snippet      string   `json:"snippet"`
	InternalDate string   `json:"internalDate"`
	Payload      struct {
		Headers []gmailHeader `json:"headers"`
	} `json:"payload"`
}

type gmailMetadataOutput struct {
	MessageID    string        `json:"message_id"`
	ThreadID     string        `json:"thread_id"`
	LabelIDs     []string      `json:"label_ids,omitempty"`
	Snippet      string        `json:"snippet,omitempty"`
	InternalDate string        `json:"internal_date,omitempty"`
	Headers      []gmailHeader `json:"headers,omitempty"`
	Receipt      string        `json:"receipt"`
}

type gmailSendInput struct {
	To      []string `json:"to" jsonschema:"primary recipient addresses"`
	Cc      []string `json:"cc,omitempty" jsonschema:"CC recipient addresses"`
	Bcc     []string `json:"bcc,omitempty" jsonschema:"BCC recipient addresses"`
	Subject string   `json:"subject" jsonschema:"message subject"`
	Body    string   `json:"body" jsonschema:"plain-text message body"`
	ReplyTo string   `json:"reply_to,omitempty" jsonschema:"optional Reply-To address"`
}

type gmailSendResponse struct {
	ID       string   `json:"id"`
	ThreadID string   `json:"threadId"`
	LabelIDs []string `json:"labelIds"`
}

type gmailSendOutput struct {
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id"`
	Receipt   string `json:"receipt"`
}

func (s *Server) gmailList(ctx context.Context, request *protocol.CallToolRequest, input gmailListInput) (*protocol.CallToolResult, gmailListOutput, error) {
	if input.MaxResults == 0 {
		input.MaxResults = 20
	}
	if input.MaxResults < 1 || input.MaxResults > 100 || len(input.LabelIDs) > 20 || len(input.PageToken) > 2048 {
		return nil, gmailListOutput{}, fmt.Errorf("Gmail list parameters exceed governed limits")
	}
	token, err := s.broker.redeem(ctx, request, gmailMetadataScope)
	if err != nil {
		return nil, gmailListOutput{}, err
	}
	query := url.Values{"maxResults": {fmt.Sprint(input.MaxResults)}}
	if input.PageToken != "" {
		query.Set("pageToken", input.PageToken)
	}
	for _, label := range input.LabelIDs {
		if strings.TrimSpace(label) == "" || len(label) > 256 {
			return nil, gmailListOutput{}, fmt.Errorf("invalid Gmail label id")
		}
		query.Add("labelIds", label)
	}
	var response gmailListResponse
	headers, err := s.google.doJSON(ctx, token, http.MethodGet, s.google.gmailBase+"/gmail/v1/users/me/messages?"+query.Encode(), nil, &response)
	if err != nil {
		return nil, gmailListOutput{}, err
	}
	receipt := "gmail:list:" + receiptCorrelation(headers)
	return providerResult(receipt, ""), gmailListOutput{
		Messages: response.Messages, NextPageToken: response.NextPageToken,
		ResultSizeEstimate: response.ResultSizeEstimate, Receipt: receipt,
	}, nil
}

func (s *Server) gmailMetadata(ctx context.Context, request *protocol.CallToolRequest, input gmailMetadataInput) (*protocol.CallToolResult, gmailMetadataOutput, error) {
	messageID := strings.TrimSpace(input.MessageID)
	if messageID == "" || len(messageID) > 512 || len(input.Headers) > 30 {
		return nil, gmailMetadataOutput{}, fmt.Errorf("invalid Gmail message metadata request")
	}
	token, err := s.broker.redeem(ctx, request, gmailMetadataScope)
	if err != nil {
		return nil, gmailMetadataOutput{}, err
	}
	query := url.Values{"format": {"metadata"}}
	for _, header := range input.Headers {
		if strings.TrimSpace(header) == "" || len(header) > 128 || strings.ContainsAny(header, "\r\n") {
			return nil, gmailMetadataOutput{}, fmt.Errorf("invalid Gmail metadata header")
		}
		query.Add("metadataHeaders", header)
	}
	var response gmailMetadataResponse
	headers, err := s.google.doJSON(ctx, token, http.MethodGet, s.google.gmailBase+"/gmail/v1/users/me/messages/"+url.PathEscape(messageID)+"?"+query.Encode(), nil, &response)
	if err != nil {
		return nil, gmailMetadataOutput{}, err
	}
	if response.ID == "" {
		return nil, gmailMetadataOutput{}, fmt.Errorf("Gmail returned no message id")
	}
	receipt := "gmail:metadata:" + response.ID + ":" + receiptCorrelation(headers)
	return providerResult(receipt, response.ID), gmailMetadataOutput{
		MessageID: response.ID, ThreadID: response.ThreadID, LabelIDs: response.LabelIDs,
		Snippet: response.Snippet, InternalDate: response.InternalDate, Headers: response.Payload.Headers, Receipt: receipt,
	}, nil
}

func (s *Server) gmailSend(ctx context.Context, request *protocol.CallToolRequest, input gmailSendInput) (*protocol.CallToolResult, gmailSendOutput, error) {
	raw, err := buildPlainTextMessage(input)
	if err != nil {
		return nil, gmailSendOutput{}, err
	}
	token, err := s.broker.redeem(ctx, request, gmailSendScope)
	if err != nil {
		return nil, gmailSendOutput{}, err
	}
	payload := map[string]string{"raw": base64.RawURLEncoding.EncodeToString(raw)}
	var response gmailSendResponse
	_, err = s.google.doJSON(ctx, token, http.MethodPost, s.google.gmailBase+"/gmail/v1/users/me/messages/send", payload, &response)
	if err != nil {
		return nil, gmailSendOutput{}, err
	}
	if strings.TrimSpace(response.ID) == "" {
		return nil, gmailSendOutput{}, fmt.Errorf("Gmail returned no provider message id")
	}
	receipt := "gmail:message:" + response.ID
	return providerResult(receipt, response.ID), gmailSendOutput{MessageID: response.ID, ThreadID: response.ThreadID, Receipt: receipt}, nil
}

func buildPlainTextMessage(input gmailSendInput) ([]byte, error) {
	if len(input.To) == 0 || len(input.To)+len(input.Cc)+len(input.Bcc) > 50 || strings.TrimSpace(input.Subject) == "" || len(input.Subject) > 998 || len(input.Body) == 0 || len(input.Body) > 1024*1024 {
		return nil, fmt.Errorf("email fields exceed governed limits")
	}
	to, err := validateAddressList(input.To)
	if err != nil {
		return nil, fmt.Errorf("invalid To address")
	}
	cc, err := validateAddressList(input.Cc)
	if err != nil {
		return nil, fmt.Errorf("invalid Cc address")
	}
	bcc, err := validateAddressList(input.Bcc)
	if err != nil {
		return nil, fmt.Errorf("invalid Bcc address")
	}
	if strings.ContainsAny(input.Subject, "\r\n") || strings.ContainsAny(input.ReplyTo, "\r\n") {
		return nil, fmt.Errorf("email headers contain a line break")
	}
	replyTo := ""
	if strings.TrimSpace(input.ReplyTo) != "" {
		replyTo, err = validateEmailAddress(input.ReplyTo)
		if err != nil {
			return nil, fmt.Errorf("invalid Reply-To address")
		}
	}
	var message strings.Builder
	fmt.Fprintf(&message, "To: %s\r\n", strings.Join(to, ", "))
	if len(cc) > 0 {
		fmt.Fprintf(&message, "Cc: %s\r\n", strings.Join(cc, ", "))
	}
	if len(bcc) > 0 {
		fmt.Fprintf(&message, "Bcc: %s\r\n", strings.Join(bcc, ", "))
	}
	if replyTo != "" {
		fmt.Fprintf(&message, "Reply-To: %s\r\n", replyTo)
	}
	body := strings.ReplaceAll(input.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	fmt.Fprintf(&message, "Subject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s", input.Subject, body)
	return []byte(message.String()), nil
}

func validateAddressList(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		normalized, err := validateEmailAddress(value)
		if err != nil {
			return nil, err
		}
		result = append(result, normalized)
	}
	return result, nil
}

func validateEmailAddress(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n") || len(value) > 320 {
		return "", fmt.Errorf("invalid email address")
	}
	address, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil || address.Address == "" {
		return "", fmt.Errorf("invalid email address")
	}
	if address.Name == "" {
		return address.Address, nil
	}
	return address.String(), nil
}

func receiptCorrelation(header http.Header) string {
	for _, name := range []string{"X-Google-Request-ID", "X-Request-ID", "ETag"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return strings.Trim(value, `"`)
		}
	}
	return "accepted"
}
