package googleworkspace

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	pluginv1 "github.com/z-chenhao/eri/api/plugin/v1"
)

const (
	calendarReadScope  = "https://www.googleapis.com/auth/calendar.events.readonly"
	calendarWriteScope = "https://www.googleapis.com/auth/calendar.events"
)

type calendarListInput struct {
	CalendarID string `json:"calendar_id,omitempty" jsonschema:"calendar id; defaults to primary"`
	TimeMin    string `json:"time_min" jsonschema:"inclusive RFC3339 lower bound"`
	TimeMax    string `json:"time_max" jsonschema:"exclusive RFC3339 upper bound"`
	TimeZone   string `json:"time_zone,omitempty" jsonschema:"IANA time zone for returned events"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"maximum events from 1 to 250"`
}

type calendarEventTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type calendarEvent struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	Summary     string            `json:"summary"`
	Description string            `json:"description,omitempty"`
	Location    string            `json:"location,omitempty"`
	HTMLLink    string            `json:"htmlLink,omitempty"`
	ETag        string            `json:"etag,omitempty"`
	Start       calendarEventTime `json:"start"`
	End         calendarEventTime `json:"end"`
}

type calendarListResponse struct {
	ETag          string          `json:"etag"`
	NextPageToken string          `json:"nextPageToken,omitempty"`
	Items         []calendarEvent `json:"items"`
}

type calendarListOutput struct {
	Events        []calendarEvent `json:"events"`
	NextPageToken string          `json:"next_page_token,omitempty"`
	Receipt       string          `json:"receipt"`
}

type calendarCreateInput struct {
	CalendarID  string   `json:"calendar_id,omitempty" jsonschema:"calendar id; defaults to primary"`
	Summary     string   `json:"summary" jsonschema:"event title"`
	Description string   `json:"description,omitempty" jsonschema:"event description"`
	Location    string   `json:"location,omitempty" jsonschema:"event location"`
	Start       string   `json:"start" jsonschema:"RFC3339 start date-time"`
	End         string   `json:"end" jsonschema:"RFC3339 end date-time"`
	TimeZone    string   `json:"time_zone,omitempty" jsonschema:"IANA time zone"`
	Attendees   []string `json:"attendees,omitempty" jsonschema:"attendee email addresses"`
	SendUpdates string   `json:"send_updates,omitempty" jsonschema:"none, all, or externalOnly"`
}

type calendarCreateOutput struct {
	EventID  string `json:"event_id"`
	Status   string `json:"status"`
	HTMLLink string `json:"html_link,omitempty"`
	Receipt  string `json:"receipt"`
}

func (s *Server) calendarList(ctx context.Context, request *protocol.CallToolRequest, input calendarListInput) (*protocol.CallToolResult, calendarListOutput, error) {
	timeMin, err := time.Parse(time.RFC3339, input.TimeMin)
	if err != nil {
		return nil, calendarListOutput{}, fmt.Errorf("time_min must be RFC3339")
	}
	timeMax, err := time.Parse(time.RFC3339, input.TimeMax)
	if err != nil || !timeMax.After(timeMin) || timeMax.Sub(timeMin) > 366*24*time.Hour {
		return nil, calendarListOutput{}, fmt.Errorf("time_max must be RFC3339, later than time_min, and within 366 days")
	}
	if err := validateTimeZone(input.TimeZone); err != nil {
		return nil, calendarListOutput{}, err
	}
	if input.MaxResults == 0 {
		input.MaxResults = 100
	}
	if input.MaxResults < 1 || input.MaxResults > 250 {
		return nil, calendarListOutput{}, fmt.Errorf("max_results must be between 1 and 250")
	}
	calendarID := strings.TrimSpace(input.CalendarID)
	if calendarID == "" {
		calendarID = "primary"
	}
	token, err := s.broker.redeem(ctx, request, calendarReadScope)
	if err != nil {
		return nil, calendarListOutput{}, err
	}
	query := url.Values{"timeMin": {input.TimeMin}, "timeMax": {input.TimeMax}, "maxResults": {fmt.Sprint(input.MaxResults)}, "singleEvents": {"true"}, "orderBy": {"startTime"}}
	if strings.TrimSpace(input.TimeZone) != "" {
		query.Set("timeZone", input.TimeZone)
	}
	var response calendarListResponse
	_, err = s.google.doJSON(ctx, token, http.MethodGet, s.google.calendarBase+"/calendar/v3/calendars/"+url.PathEscape(calendarID)+"/events?"+query.Encode(), nil, &response)
	if err != nil {
		return nil, calendarListOutput{}, err
	}
	receipt := "google-calendar:list:" + strings.Trim(response.ETag, `"`)
	output := calendarListOutput{Events: response.Items, NextPageToken: response.NextPageToken, Receipt: receipt}
	return providerResult(receipt, ""), output, nil
}

func (s *Server) calendarCreate(ctx context.Context, request *protocol.CallToolRequest, input calendarCreateInput) (*protocol.CallToolResult, calendarCreateOutput, error) {
	start, err := time.Parse(time.RFC3339, input.Start)
	if err != nil {
		return nil, calendarCreateOutput{}, fmt.Errorf("start must be RFC3339")
	}
	end, err := time.Parse(time.RFC3339, input.End)
	if err != nil || !end.After(start) {
		return nil, calendarCreateOutput{}, fmt.Errorf("end must be RFC3339 and later than start")
	}
	if strings.TrimSpace(input.Summary) == "" || len(input.Summary) > 1024 || len(input.Description) > 8192 || len(input.Location) > 1024 || len(input.Attendees) > 20 {
		return nil, calendarCreateOutput{}, fmt.Errorf("calendar event fields exceed governed limits")
	}
	if err := validateTimeZone(input.TimeZone); err != nil {
		return nil, calendarCreateOutput{}, err
	}
	if input.SendUpdates == "" {
		input.SendUpdates = "none"
	}
	if input.SendUpdates != "none" && input.SendUpdates != "all" && input.SendUpdates != "externalOnly" {
		return nil, calendarCreateOutput{}, fmt.Errorf("send_updates must be none, all, or externalOnly")
	}
	attendees := make([]map[string]string, 0, len(input.Attendees))
	for _, address := range input.Attendees {
		normalized, err := validateEmailAddress(address)
		if err != nil {
			return nil, calendarCreateOutput{}, fmt.Errorf("invalid attendee address")
		}
		attendees = append(attendees, map[string]string{"email": normalized})
	}
	calendarID := strings.TrimSpace(input.CalendarID)
	if calendarID == "" {
		calendarID = "primary"
	}
	token, err := s.broker.redeem(ctx, request, calendarWriteScope)
	if err != nil {
		return nil, calendarCreateOutput{}, err
	}
	payload := map[string]any{
		"summary": input.Summary, "description": input.Description, "location": input.Location,
		"start": calendarEventTime{DateTime: input.Start, TimeZone: input.TimeZone},
		"end":   calendarEventTime{DateTime: input.End, TimeZone: input.TimeZone}, "attendees": attendees,
	}
	var response calendarEvent
	query := url.Values{"sendUpdates": {input.SendUpdates}}
	_, err = s.google.doJSON(ctx, token, http.MethodPost, s.google.calendarBase+"/calendar/v3/calendars/"+url.PathEscape(calendarID)+"/events?"+query.Encode(), payload, &response)
	if err != nil {
		return nil, calendarCreateOutput{}, err
	}
	if strings.TrimSpace(response.ID) == "" {
		return nil, calendarCreateOutput{}, fmt.Errorf("Google Calendar returned no event id")
	}
	receipt := "google-calendar:event:" + response.ID + ":" + strings.Trim(response.ETag, `"`)
	output := calendarCreateOutput{EventID: response.ID, Status: response.Status, HTMLLink: response.HTMLLink, Receipt: receipt}
	return providerResult(receipt, response.ID), output, nil
}

func validateTimeZone(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > 128 {
		return fmt.Errorf("time_zone must be a valid IANA time zone")
	}
	if _, err := time.LoadLocation(value); err != nil {
		return fmt.Errorf("time_zone must be a valid IANA time zone")
	}
	return nil
}

func providerResult(receipt, externalObjectID string) *protocol.CallToolResult {
	return &protocol.CallToolResult{Meta: protocol.Meta{pluginv1.ResultMetadataKey: pluginv1.ResultMetadata{
		Receipt: receipt, ExternalObjectID: externalObjectID, FreshAt: time.Now().UTC(),
	}}}
}
