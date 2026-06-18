package notify

import "errors"

// ErrEmptyWebhook reports a Notify call on a notifier built without a webhook
// URL. The CLI guards against this by returning no notifier when the env var is
// unset, so it is a defensive sentinel rather than an expected path.
var ErrEmptyWebhook = errors.New("notify: empty webhook url")

// ErrNotifyStatus reports the webhook endpoint answered with a non-2xx status,
// so the caller can log a failed delivery without parsing the body.
var ErrNotifyStatus = errors.New("notify: webhook returned error status")
