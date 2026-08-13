package internal

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

type NeedsReplyCandidatesConfig struct {
	DB             *sql.DB
	WorkspaceID    string
	CampaignID     int64
	Since          time.Time
	Now            time.Time
	Limit          int
	IncludeThread  bool
	Reconcile      bool
	SecretResolver SecretResolver
	GWS            GWSClient
	IMAP           IMAPMessageLister
}

type FindNeedsReplyCandidatesConfig struct {
	DB            *sql.DB
	WorkspaceID   string
	CampaignID    int64
	Since         time.Time
	Now           time.Time
	Limit         int
	IncludeThread bool
}

type NeedsReplyCandidate struct {
	Rank                 int                     `json:"rank"`
	CampaignID           int64                   `json:"campaign_id"`
	CampaignName         string                  `json:"campaign_name"`
	CampaignStatus       string                  `json:"campaign_status"`
	LeadID               int64                   `json:"lead_id"`
	LeadStatus           string                  `json:"lead_status"`
	LeadEmail            string                  `json:"lead_email"`
	Company              string                  `json:"company,omitempty"`
	AccountID            int64                   `json:"account_id"`
	FromEmail            string                  `json:"from_email"`
	ToEmail              string                  `json:"to_email"`
	Subject              string                  `json:"subject"`
	ThreadID             string                  `json:"thread_id"`
	ReplyCount           int                     `json:"reply_count"`
	MessageCount         int                     `json:"message_count"`
	AgeHours             int                     `json:"age_hours"`
	LastInboundAt        time.Time               `json:"last_inbound_at"`
	LastInboundFrom      string                  `json:"last_inbound_from"`
	LastInboundBody      string                  `json:"last_inbound_body"`
	PreviousOutboundAt   time.Time               `json:"previous_outbound_at,omitempty"`
	PreviousOutboundBody string                  `json:"previous_outbound_body,omitempty"`
	Thread               []FollowupThreadMessage `json:"thread,omitempty"`
}

type NeedsReplyCandidatesResult struct {
	WorkspaceID    string                `json:"workspace_id"`
	Since          time.Time             `json:"since"`
	Audit          *InboxAuditResult     `json:"audit"`
	Reconciliation *InboxReconcileResult `json:"reconciliation,omitempty"`
	Candidates     []NeedsReplyCandidate `json:"candidates"`
}

type needsReplyCandidateHead struct {
	Latest         EmailMessage
	CampaignName   string
	CampaignStatus string
	LeadStatus     string
	LeadEmail      string
	Company        string
	AccountEmail   string
}

// ReviewNeedsReplyCandidates makes provider state authoritative before
// returning threads whose newest message is a human inbound reply. It never
// drafts or sends. Reconcile is the only mode that writes.
func ReviewNeedsReplyCandidates(cfg NeedsReplyCandidatesConfig) (*NeedsReplyCandidatesResult, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	workspaceID := NormalizeWorkspaceID(cfg.WorkspaceID)
	if cfg.Now.IsZero() {
		cfg.Now = time.Now().UTC()
	}
	if cfg.Since.IsZero() {
		cfg.Since = cfg.Now.AddDate(0, 0, -120)
	}
	result := &NeedsReplyCandidatesResult{WorkspaceID: workspaceID, Since: cfg.Since.UTC()}
	auditCfg := AuditInboxHistoryConfig{
		DB: cfg.DB, WorkspaceID: workspaceID, Since: cfg.Since,
		SecretResolver: cfg.SecretResolver, GWS: cfg.GWS, IMAP: cfg.IMAP,
	}
	if cfg.Reconcile {
		reconciliation, err := ReconcileInboxHistory(auditCfg)
		result.Reconciliation = reconciliation
		if reconciliation != nil {
			result.Audit = reconciliation.Verification
		}
		if err != nil {
			return result, err
		}
	} else {
		audit, err := AuditInboxHistory(auditCfg)
		result.Audit = audit
		if err != nil {
			return result, fmt.Errorf("provider audit failed; reply review blocked: %w", err)
		}
		if audit.Missing != 0 {
			return result, fmt.Errorf("provider reconciliation required: %d campaign-thread messages are untracked; rerun with --reconcile", audit.Missing)
		}
	}

	candidates, err := FindNeedsReplyCandidates(FindNeedsReplyCandidatesConfig{
		DB: cfg.DB, WorkspaceID: workspaceID, CampaignID: cfg.CampaignID,
		Since: cfg.Since, Now: cfg.Now, Limit: cfg.Limit, IncludeThread: cfg.IncludeThread,
	})
	if err != nil {
		return result, err
	}
	result.Candidates = candidates
	return result, nil
}

func FindNeedsReplyCandidates(cfg FindNeedsReplyCandidatesConfig) ([]NeedsReplyCandidate, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	if cfg.Now.IsZero() {
		cfg.Now = time.Now().UTC()
	}
	if cfg.Since.IsZero() {
		cfg.Since = cfg.Now.AddDate(0, 0, -120)
	}
	limit := cfg.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	query := `
		SELECT em.campaign_id, em.lead_id, em.account_id, em.message_id, em.thread_id,
			em.in_reply_to, em.from_email, em.to_emails, em.subject, em.text_body,
			em.display_body, em.snippet, em.raw_headers, em.occurred_at,
			c.name, c.status, cl.status, l.email, l.company, a.email
		FROM email_messages em
		JOIN campaigns c ON c.id = em.campaign_id
		JOIN campaign_leads cl ON cl.campaign_id = em.campaign_id AND cl.lead_id = em.lead_id
		JOIN leads l ON l.id = em.lead_id
		JOIN accounts a ON a.id = em.account_id
		WHERE c.workspace_id = ?
		AND em.id = (
			SELECT em2.id FROM email_messages em2
			WHERE em2.campaign_id = em.campaign_id AND em2.lead_id = em.lead_id
			ORDER BY em2.occurred_at DESC, em2.id DESC LIMIT 1
		)
		AND em.direction = 'inbound'
		AND em.type = 'reply'
		AND em.occurred_at >= ?
		AND em.thread_id <> ''
		AND cl.status = 'replied'
		AND l.global_status = 'active'
		AND a.status = 'active'
		AND NOT EXISTS (
			SELECT 1 FROM events suppressed
			WHERE suppressed.campaign_id = em.campaign_id AND suppressed.lead_id = em.lead_id
			AND suppressed.type IN ('unsubscribe', 'bounce')
		)`
	args := []any{NormalizeWorkspaceID(cfg.WorkspaceID), cfg.Since.UTC()}
	if cfg.CampaignID > 0 {
		query += " AND em.campaign_id = ?"
		args = append(args, cfg.CampaignID)
	}
	query += " ORDER BY em.occurred_at ASC, em.id ASC"

	rows, err := queryDB(cfg.DB, query, args...)
	if err != nil {
		return nil, fmt.Errorf("loading needs-reply candidate heads: %w", err)
	}
	defer rows.Close()

	var heads []needsReplyCandidateHead
	for rows.Next() {
		var head needsReplyCandidateHead
		head.Latest.Direction = EmailMessageDirectionInbound
		head.Latest.Type = EmailMessageTypeReply
		if err := rows.Scan(
			&head.Latest.CampaignID, &head.Latest.LeadID, &head.Latest.AccountID,
			&head.Latest.MessageID, &head.Latest.ThreadID, &head.Latest.InReplyTo,
			&head.Latest.FromEmail, &head.Latest.ToEmails, &head.Latest.Subject,
			&head.Latest.TextBody, &head.Latest.DisplayBody, &head.Latest.Snippet,
			&head.Latest.RawHeaders, &head.Latest.OccurredAt, &head.CampaignName,
			&head.CampaignStatus, &head.LeadStatus, &head.LeadEmail, &head.Company,
			&head.AccountEmail,
		); err != nil {
			return nil, err
		}
		heads = append(heads, head)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	candidates := make([]NeedsReplyCandidate, 0, len(heads))
	for _, head := range heads {
		messages, err := ListEmailThreadMessages(cfg.DB, ListEmailThreadMessagesOpts{
			CampaignID: head.Latest.CampaignID, LeadID: head.Latest.LeadID,
			Limit: 500,
		})
		if err != nil {
			return nil, fmt.Errorf("loading campaign %d lead %d thread: %w", head.Latest.CampaignID, head.Latest.LeadID, err)
		}
		candidate, err := needsReplyCandidateFromThread(cfg.DB, cfg.Now, cfg.IncludeThread, head, messages)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].LastInboundAt.Before(candidates[j].LastInboundAt)
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	for index := range candidates {
		candidates[index].Rank = index + 1
	}
	return candidates, nil
}

func needsReplyCandidateFromThread(db *sql.DB, now time.Time, includeThread bool, head needsReplyCandidateHead, messages []EmailMessage) (NeedsReplyCandidate, error) {
	latest := head.Latest
	toEmail, err := replyRecipientEmail(db, latest.LeadID, latest)
	if err != nil {
		return NeedsReplyCandidate{}, fmt.Errorf("resolving campaign %d lead %d recipient: %w", latest.CampaignID, latest.LeadID, err)
	}
	replyCount := 0
	var previousOutbound EmailMessage
	for _, message := range messages {
		if message.Direction == EmailMessageDirectionInbound && message.Type == EmailMessageTypeReply {
			replyCount++
		}
		if message.OccurredAt.Before(latest.OccurredAt) && message.Direction == EmailMessageDirectionOutbound &&
			(message.Type == EmailMessageTypeSent || message.Type == EmailMessageTypeManualReply) {
			previousOutbound = message
		}
	}
	age := now.Sub(latest.OccurredAt)
	if age < 0 {
		age = 0
	}
	var thread []FollowupThreadMessage
	if includeThread {
		thread = make([]FollowupThreadMessage, 0, len(messages))
		for _, message := range messages {
			thread = append(thread, FollowupThreadMessage{
				Direction: message.Direction, Type: message.Type, FromEmail: message.FromEmail,
				ToEmails: message.ToEmails, Subject: message.Subject,
				Body: followupMessageBody(message), OccurredAt: message.OccurredAt,
			})
		}
	}
	return NeedsReplyCandidate{
		CampaignID: latest.CampaignID, CampaignName: head.CampaignName,
		CampaignStatus: head.CampaignStatus, LeadID: latest.LeadID,
		LeadStatus: head.LeadStatus, LeadEmail: head.LeadEmail, Company: head.Company,
		AccountID: latest.AccountID, FromEmail: head.AccountEmail, ToEmail: toEmail,
		Subject: latest.Subject, ThreadID: latest.ThreadID, ReplyCount: replyCount,
		MessageCount: len(messages), AgeHours: int(age / time.Hour),
		LastInboundAt: latest.OccurredAt, LastInboundFrom: latest.FromEmail,
		LastInboundBody:      followupMessageBody(latest),
		PreviousOutboundAt:   previousOutbound.OccurredAt,
		PreviousOutboundBody: strings.TrimSpace(followupMessageBody(previousOutbound)),
		Thread:               thread,
	}, nil
}
