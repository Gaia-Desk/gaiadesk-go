package gaiadesk

import "encoding/json"

// The results below are the API's JSON shapes, which are gaiadesk-cli's own
// (`--json`), from GaiaDesk's JSON Schema (client/schema.json). Field names
// follow the wire's snake_case in their JSON tags. A nullable number is a
// pointer; a nullable string is "" when null.

// CliError is what went wrong, as the API and gaiadesk-cli report it: the
// object inside `{"error": …}` (and exec's `error`).
type CliError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
	Desk    string `json:"desk,omitempty"`
}

// ───────────────────────────── desks ─────────────────────────────

// Identity is who the caller is: `{source, account}`.
type Identity struct {
	Source  string `json:"source"`
	Account string `json:"account,omitempty"`
}

// ReachSuccess is a desk's last successful connection, as the CLI keeps it.
type ReachSuccess struct {
	At        int64   `json:"at"`
	Route     string  `json:"route"`
	ConnectMS *uint64 `json:"connect_ms,omitempty"`
	RTTMS     *uint64 `json:"rtt_ms,omitempty"`
}

// ReachFailure is a desk's last failed connection, as the CLI keeps it.
type ReachFailure struct {
	At      int64  `json:"at"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Desk is one desk as `GET /desks` lists it: the CLI's `devices --json`
// row plus the reach log's word on an offline one and its end-to-end key.
type Desk struct {
	DeskID         string        `json:"desk_id"`
	Name           string        `json:"name,omitempty"`
	Online         bool          `json:"online"`
	OS             string        `json:"os,omitempty"`
	AppVersion     string        `json:"app_version,omitempty"`
	Owner          string        `json:"owner,omitempty"`
	Sources        []string      `json:"sources"`
	LastSeen       *int64        `json:"last_seen,omitempty"`
	SignalIdleSecs *uint64       `json:"signal_idle_secs,omitempty"`
	Anytime        *bool         `json:"anytime,omitempty"`
	Reachable      *bool         `json:"reachable,omitempty"`
	LastOK         *ReachSuccess `json:"last_ok,omitempty"`
	LastFailure    *ReachFailure `json:"last_failure,omitempty"`
	// OfflineSince is when it went offline (Unix seconds), with
	// OfflineReason (`closed`, `silent`, `error`, `updating`,
	// `server-restart`, `id-changed`, `unknown`) and its text.
	OfflineSince      *int64 `json:"offline_since,omitempty"`
	OfflineReason     string `json:"offline_reason,omitempty"`
	OfflineReasonText string `json:"offline_reason_text,omitempty"`
	OfflineDetail     string `json:"offline_detail,omitempty"`
	// Features is what it takes now, while online: `desk_op`, `desk_op_e2e`.
	Features []string `json:"features,omitempty"`
	// E2EPub is its end-to-end X25519 public key (base64url) while online
	// and able to open sealed operations.
	E2EPub string `json:"e2e_pub,omitempty"`
	// E2ERequired: its owner requires end-to-end encryption for API commands.
	E2ERequired bool `json:"e2e_required,omitempty"`
}

// DeskList is `GET /desks`: the CLI's `devices --json` object.
type DeskList struct {
	Devices  []Desk    `json:"devices"`
	Sources  []string  `json:"sources"`
	Notes    []string  `json:"notes"`
	Identity *Identity `json:"identity,omitempty"`
}

// WakeHints say how a desk could be woken now.
type WakeHints struct {
	DoorbellSockets int  `json:"doorbell_sockets"`
	LANWake         bool `json:"lan_wake"`
}

// DeskDetail is `GET /desks/{id}`: one desk, with its wake hints.
type DeskDetail struct {
	Desk
	Wake *WakeHints `json:"wake,omitempty"`
}

// ReachEvent is one online/offline transition of a desk.
type ReachEvent struct {
	At         int64  `json:"at"`
	Online     bool   `json:"online"`
	Reason     string `json:"reason"`
	ReasonText string `json:"reason_text"`
	Detail     string `json:"detail,omitempty"`
	Version    string `json:"version,omitempty"`
}

// ReachLog is `GET /desks/{id}/reach`: transitions, newest first.
type ReachLog struct {
	DeskID string       `json:"desk_id"`
	Since  int64        `json:"since"`
	Events []ReachEvent `json:"events"`
}

// WakeResult is `POST /desks/{id}/wake`.
type WakeResult struct {
	DeskID        string `json:"desk_id"`
	Online        bool   `json:"online"`
	Woke          bool   `json:"woke"`
	AlreadyOnline bool   `json:"already_online"`
	Rang          struct {
		Doorbell   int `json:"doorbell"`
		LANHelpers int `json:"lan_helpers"`
	} `json:"rang"`
	WaitedMS int64 `json:"waited_ms"`
}

// ───────────────────────────── exec ─────────────────────────────

// ExecResult is how a command ended: `exec --json`. Exit is what
// gaiadesk-cli exits with (the command's code, 124 timed out, 130
// interrupted, 254 refused); Error is nil when the command ran and ended on
// its own.
type ExecResult struct {
	Exit       int       `json:"exit"`
	RemoteCode *int      `json:"remote_code"`
	Stdout     string    `json:"stdout"`
	Stderr     string    `json:"stderr"`
	DurationMS uint64    `json:"duration_ms"`
	Desk       string    `json:"desk"`
	Route      string    `json:"route,omitempty"`
	Mode       string    `json:"mode,omitempty"`
	Shell      string    `json:"shell,omitempty"`
	TimedOut   bool      `json:"timed_out"`
	Truncated  bool      `json:"truncated"`
	Notes      []string  `json:"notes"`
	Error      *CliError `json:"error"`
}

// ExecExit is the end of a streamed run, without its output.
type ExecExit struct {
	Exit       int       `json:"exit"`
	RemoteCode *int      `json:"remote_code"`
	DurationMS uint64    `json:"duration_ms"`
	Desk       string    `json:"desk"`
	Route      string    `json:"route,omitempty"`
	Mode       string    `json:"mode,omitempty"`
	Shell      string    `json:"shell,omitempty"`
	TimedOut   bool      `json:"timed_out"`
	Notes      []string  `json:"notes"`
	Error      *CliError `json:"error"`
}

// ───────────────────────────── jobs ─────────────────────────────

// JobLimits are a job's resource caps.
type JobLimits struct {
	Priority   string  `json:"priority,omitempty"`
	CPUPercent *uint32 `json:"cpu_percent,omitempty"`
	MemMB      *uint64 `json:"mem_mb,omitempty"`
	KeepAwake  *bool   `json:"keep_awake,omitempty"`
}

// Job is a background job.
type Job struct {
	Name        string     `json:"name"`
	Command     string     `json:"command"`
	State       string     `json:"state"`
	StartedAtMS uint64     `json:"started_at_ms"`
	EndedAtMS   *uint64    `json:"ended_at_ms,omitempty"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	PID         *uint32    `json:"pid,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	By          string     `json:"by,omitempty"`
	LogBytes    uint64     `json:"log_bytes,omitempty"`
	Limits      *JobLimits `json:"limits,omitempty"`
	Enforcement []string   `json:"enforcement,omitempty"`
}

// JobLogs is a job and the end of its output.
type JobLogs struct {
	Job    *Job   `json:"job,omitempty"`
	Output string `json:"output"`
}

// JobWaitResult is the job as it ended or, TimedOut, as it stands, still
// running. Its ExitCode is a result, not an error.
type JobWaitResult struct {
	Job      Job  `json:"job"`
	TimedOut bool `json:"timed_out"`
}

// ───────────────────────────── stats ─────────────────────────────

// DiskStat is one disk of a desk.
type DiskStat struct {
	Mount   string `json:"mount"`
	TotalMB uint64 `json:"total_mb"`
	FreeMB  uint64 `json:"free_mb"`
}

// StatsReport is a desk's figures: `stats --json`.
type StatsReport struct {
	Desk        string     `json:"desk"`
	Hostname    string     `json:"hostname"`
	OS          string     `json:"os"`
	OSVersion   string     `json:"os_version,omitempty"`
	CPUPercent  float64    `json:"cpu_percent"`
	CPUs        uint32     `json:"cpus"`
	Load        []float64  `json:"load,omitempty"`
	MemTotalMB  uint64     `json:"mem_total_mb"`
	MemFreeMB   uint64     `json:"mem_free_mb"`
	UptimeSecs  uint64     `json:"uptime_secs"`
	JobsRunning uint32     `json:"jobs_running"`
	Disks       []DiskStat `json:"disks,omitempty"`
}

// ───────────────────────────── files ─────────────────────────────

// CopyFailure is one file that failed to copy.
type CopyFailure struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// CopyResult is what a copy did: `cp --json`.
type CopyResult struct {
	Direction    string        `json:"direction"`
	Desk         string        `json:"desk"`
	Destination  string        `json:"destination"`
	Files        uint32        `json:"files"`
	Dirs         uint32        `json:"dirs"`
	Bytes        uint64        `json:"bytes"`
	ResumedBytes uint64        `json:"resumed_bytes"`
	Failed       []CopyFailure `json:"failed"`
	Seconds      float64       `json:"seconds"`
}

// ───────────────────────────── tokens ─────────────────────────────

// TokenInfo is an agent token as the desk describes it (never its secret).
type TokenInfo struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	IssuedAtMS  uint64   `json:"issued_at_ms"`
	ExpiresAtMS uint64   `json:"expires_at_ms"`
	LastUsedMS  *uint64  `json:"last_used_ms,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
	Cwd         string   `json:"cwd,omitempty"`
	LowPriv     bool     `json:"low_priv,omitempty"`
	Revoked     bool     `json:"revoked,omitempty"`
}

// MintedToken is one desk's new token and its secret (shown once).
type MintedToken struct {
	Desk   string    `json:"desk"`
	Token  TokenInfo `json:"token"`
	Secret string    `json:"secret"`
}

// MintResult is [Client.CreateToken]: one token per desk.
type MintResult struct {
	Tokens []MintedToken `json:"tokens"`
}

// Revoked is `DELETE /desks/{id}/tokens/{token_id}`.
type Revoked struct {
	Revoked         string `json:"revoked"`
	StoppedSessions uint32 `json:"stopped_sessions"`
}

// ───────────────────────────── audit ─────────────────────────────

// AuditEvent is one audit event (Atlas audit events).
type AuditEvent struct {
	ID           string `json:"id"`
	Action       string `json:"action"`
	Stream       string `json:"stream"`
	OccurredAtMS int64  `json:"occurred_at_ms"`
	Actor        struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
	} `json:"actor"`
	Target *struct {
		Type string `json:"type,omitempty"`
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"target,omitempty"`
	Metadata json.RawMessage `json:"metadata"`
}

// ───────────────────────────── webhooks ─────────────────────────────

// Webhook is a webhook subscription (never its secret).
type Webhook struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	Description string   `json:"description"`
	CreatedAt   int64    `json:"created_at"`
}

// WebhookCreated is a new subscription with its signing secret (shown once).
type WebhookCreated struct {
	Webhook
	Secret string `json:"secret"`
}

// ───────────────────────────── support ─────────────────────────────

// SupportSession is a support session for the web embed SDK.
type SupportSession struct {
	ID               string         `json:"id"`
	State            string         `json:"state"`
	Mode             string         `json:"mode"`
	Customer         map[string]any `json:"customer"`
	CustomerPresent  bool           `json:"customer_present"`
	CustomerVerified bool           `json:"customer_verified"`
	JoinCode         string         `json:"join_code"`
	JoinURL          string         `json:"join_url"`
	DeskID           string         `json:"desk_id,omitempty"`
	Origin           string         `json:"origin,omitempty"`
	Owner            string         `json:"owner"`
	CreatedAt        int64          `json:"created_at"`
	ExpiresAt        int64          `json:"expires_at"`
	JoinedAt         *int64         `json:"joined_at,omitempty"`
	JoinedBy         string         `json:"joined_by,omitempty"`
	EndedAt          *int64         `json:"ended_at,omitempty"`
	EndReason        string         `json:"end_reason,omitempty"`
}

// SupportSessionCreated is a new session with its embed token (shown once).
type SupportSessionCreated struct {
	SupportSession
	EmbedToken string `json:"embed_token"`
}
