package main

import "time"

const (
	pluginName               = "quota-router"
	defaultCutoffPercentUsed = 50.0
	defaultProtectedModel    = "claude-fable-5"
	defaultUsageScope        = "seven_day"
	fiveHourUsageScope       = "five_hour"
	allModelsSelector        = "*"
	reserveErrorCode         = "quota_router_reserve"
	defaultPollInterval      = 5 * time.Minute
	defaultNearPollInterval  = time.Minute
	defaultRequestTimeout    = 10 * time.Second
	defaultUsageEndpoint     = "https://api.anthropic.com/api/oauth/usage"
	anthropicOAuthBeta       = "oauth-2025-04-20"
	maxUsageResponseBytes    = 64 << 10
	exhaustedErrorCode       = "quota_router_exhausted"
	managementStatusRoute    = "/plugins/quota-router/status"
	managementStatusFullPath = "/v0/management" + managementStatusRoute
	pollErrorAuthGet         = "auth_get"
	pollErrorMissingToken    = "missing_token"
	pollErrorInvalidWeekly   = "invalid_weekly"
	pollErrorInvalidScope    = "invalid_scope"
	pollErrorInvalidJSON     = "invalid_json"
	pollErrorBodyTooLarge    = "body_too_large"
	pollErrorTimeout         = "timeout"
	pollErrorCancelled       = "cancelled"
	pollErrorNetwork         = "network"
	pollErrorUnauthorized    = "unauthorized"
	pollErrorForbidden       = "forbidden"
	pollErrorRateLimited     = "rate_limited"
	pollErrorServer          = "server_error"
	pollErrorHTTP            = "http_error"
	pollErrorRead            = "read_error"

	usageSourceEndpoint      = "endpoint"
	usageSourceSeatPacerFile = "seat-pacer-file"
	defaultSeatPacerFile     = "~/.cli-proxy-api/plugins/claude-seat-pacer/history.json"
	maxSeatPacerFileBytes    = 4 << 20
	// seatPacerMinReread spaces the file reads a refused request asks for, so
	// a burst of refusals reads the file once.
	seatPacerMinReread = 5 * time.Second

	pollErrorSeatPacerFileMissing    = "seat_pacer_file_missing"
	pollErrorSeatPacerFileUnreadable = "seat_pacer_file_unreadable"
	pollErrorSeatPacerFileInvalid    = "seat_pacer_file_invalid"
	pollErrorSeatPacerFileVersion    = "seat_pacer_file_version"
	pollErrorSeatPacerNoReading      = "seat_pacer_no_reading"
)

const disabledNearCutoffPercent = -1.0

var pluginVersion = "0.9.0"

// Variable only so the C-shared integration test can inject a local fixture.
var usageEndpoint = defaultUsageEndpoint

var activeRuntime = newPluginRuntime(
	cgoHostClient{},
	newHTTPUsageFetcher(usageEndpoint, nil).fetch,
	time.Now,
)

func main() {}
