package internal

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const (
	CampaignHistoryScopeAll       = "all"
	CampaignHistoryScopeWorkspace = "workspace"

	CampaignPreflightReady        = "ready"
	CampaignPreflightManualReview = "manual_review"
	CampaignPreflightBlocked      = "blocked"
)

type CampaignPreflightConfig struct {
	DB                  *sql.DB
	WorkspaceID         string
	HistoryScope        string
	CheckDomains        bool
	Records             []LeadRecord
	SkipEmailValidation bool
	EmailVerifier       RecipientVerifier
	ValidationPolicy    EmailValidationPolicy
}

type CampaignPreflightHistoryMatch struct {
	MatchType        string `json:"match_type"`
	LeadEmail        string `json:"lead_email"`
	LeadGlobalStatus string `json:"lead_global_status"`
	CampaignID       int64  `json:"campaign_id,omitempty"`
	CampaignName     string `json:"campaign_name,omitempty"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
	CampaignStatus   string `json:"campaign_status,omitempty"`
	CampaignLead     string `json:"campaign_lead_status,omitempty"`
}

type CampaignPreflightRow struct {
	Email            string                          `json:"email"`
	Domain           string                          `json:"domain"`
	Status           string                          `json:"status"`
	Reasons          []string                        `json:"reasons,omitempty"`
	ValidationStatus string                          `json:"validation_status,omitempty"`
	SMTPStatus       string                          `json:"smtp_status,omitempty"`
	ValidationDetail string                          `json:"validation_detail,omitempty"`
	History          []CampaignPreflightHistoryMatch `json:"history,omitempty"`
}

type CampaignPreflightResult struct {
	WorkspaceID  string                 `json:"workspace_id"`
	HistoryScope string                 `json:"history_scope"`
	CheckDomains bool                   `json:"check_domains"`
	EmailChecks  bool                   `json:"email_checks"`
	Checked      int                    `json:"checked"`
	Ready        int                    `json:"ready"`
	ManualReview int                    `json:"manual_review"`
	Blocked      int                    `json:"blocked"`
	Rows         []CampaignPreflightRow `json:"rows"`
}

func (r CampaignPreflightResult) HasBlockingRows() bool {
	return r.ManualReview > 0 || r.Blocked > 0
}

// PreflightCampaignLeads combines input duplicate checks, cross-campaign
// history checks, global suppressions, and optional recipient validation. It is
// read-only and is intended to run before create, clone, or add-leads.
func PreflightCampaignLeads(cfg CampaignPreflightConfig) (*CampaignPreflightResult, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	if len(cfg.Records) == 0 {
		return nil, fmt.Errorf("records are required")
	}
	historyScope := strings.ToLower(strings.TrimSpace(cfg.HistoryScope))
	if historyScope == "" {
		historyScope = CampaignHistoryScopeAll
	}
	if historyScope != CampaignHistoryScopeAll && historyScope != CampaignHistoryScopeWorkspace {
		return nil, fmt.Errorf("history_scope must be %q or %q", CampaignHistoryScopeAll, CampaignHistoryScopeWorkspace)
	}
	workspaceID := NormalizeWorkspaceID(cfg.WorkspaceID)
	result := &CampaignPreflightResult{
		WorkspaceID:  workspaceID,
		HistoryScope: historyScope,
		CheckDomains: cfg.CheckDomains,
		EmailChecks:  !cfg.SkipEmailValidation,
		Checked:      len(cfg.Records),
		Rows:         make([]CampaignPreflightRow, len(cfg.Records)),
	}

	emailCounts := map[string]int{}
	domainCounts := map[string]int{}
	for _, record := range cfg.Records {
		email := normalizePreflightEmail(record.Fields["email"])
		domain := ExtractDomain(email)
		emailCounts[email]++
		if cfg.CheckDomains && !smtpSkippedRecipientDomains[domain] {
			domainCounts[domain]++
		}
	}

	var validationRecords []LeadRecord
	validationIndexes := map[string]int{}
	for index, record := range cfg.Records {
		email := normalizePreflightEmail(record.Fields["email"])
		domain := ExtractDomain(email)
		row := CampaignPreflightRow{Email: email, Domain: domain, Status: CampaignPreflightReady}
		if emailCounts[email] > 1 {
			row.Reasons = append(row.Reasons, "duplicate_input_email: recipient appears more than once in the candidate CSV")
		}
		if cfg.CheckDomains && !smtpSkippedRecipientDomains[domain] && domainCounts[domain] > 1 {
			row.Reasons = append(row.Reasons, "duplicate_input_domain: more than one candidate uses this company email domain")
		}

		history, err := loadCampaignPreflightHistory(cfg.DB, workspaceID, historyScope, email, domain, cfg.CheckDomains)
		if err != nil {
			return nil, fmt.Errorf("checking history for %s: %w", email, err)
		}
		row.History = history
		for _, match := range history {
			if match.MatchType == "email" && match.LeadGlobalStatus != "" && match.LeadGlobalStatus != "active" {
				row.Reasons = appendUniqueString(row.Reasons, "global_suppression: matching lead is "+match.LeadGlobalStatus)
			}
			if match.MatchType == "email" {
				if match.CampaignID > 0 {
					row.Reasons = appendUniqueString(row.Reasons, "prior_email_campaign: recipient already belongs to a campaign")
				}
			}
			if match.MatchType == "domain" {
				if match.CampaignID > 0 {
					row.Reasons = appendUniqueString(row.Reasons, "prior_domain_campaign: another recipient at this domain already belongs to a campaign")
				}
			}
		}

		if len(row.Reasons) > 0 {
			row.Status = CampaignPreflightBlocked
		} else if !cfg.SkipEmailValidation {
			validationRecords = append(validationRecords, LeadRecord{Fields: map[string]string{"email": email}})
			validationIndexes[email] = index
		}
		result.Rows[index] = row
	}

	if !cfg.SkipEmailValidation && len(validationRecords) > 0 {
		validation, err := ValidateLeadEmails(validationRecords, cfg.EmailVerifier, cfg.ValidationPolicy)
		if err != nil {
			return nil, fmt.Errorf("validating recipient emails: %w", err)
		}
		for _, validationRow := range validation.Rows {
			index, ok := validationIndexes[normalizePreflightEmail(validationRow.Email)]
			if !ok {
				continue
			}
			row := &result.Rows[index]
			row.ValidationStatus = validationRow.ValidationStatus
			row.SMTPStatus = validationRow.SMTPStatus
			row.ValidationDetail = validationRow.Detail
			switch validationRow.ValidationStatus {
			case EmailValidationPass:
				row.Status = CampaignPreflightReady
			case EmailValidationManualReview:
				row.Status = CampaignPreflightManualReview
				row.Reasons = appendUniqueString(row.Reasons, "email_manual_review: "+validationRow.Detail)
			default:
				row.Status = CampaignPreflightBlocked
				row.Reasons = appendUniqueString(row.Reasons, "email_validation_failed: "+validationRow.Detail)
			}
		}
	}

	for _, row := range result.Rows {
		switch row.Status {
		case CampaignPreflightReady:
			result.Ready++
		case CampaignPreflightManualReview:
			result.ManualReview++
		default:
			result.Blocked++
		}
	}
	return result, nil
}

func loadCampaignPreflightHistory(db *sql.DB, workspaceID, historyScope, email, domain string, checkDomains bool) ([]CampaignPreflightHistoryMatch, error) {
	query := `
		SELECT l.email, l.domain, l.global_status,
			COALESCE(c.id, 0), COALESCE(c.name, ''), COALESCE(c.workspace_id, ''),
			COALESCE(c.status, ''), COALESCE(cl.status, '')
		FROM leads l
		LEFT JOIN campaign_leads cl ON cl.lead_id = l.id
		LEFT JOIN campaigns c ON c.id = cl.campaign_id
		WHERE LOWER(l.email) = ?`
	args := []any{email}
	if checkDomains && !smtpSkippedRecipientDomains[domain] {
		query += " OR LOWER(l.domain) = ?"
		args = append(args, domain)
	}
	query += " ORDER BY l.email, c.id"
	rows, err := queryDB(db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matches []CampaignPreflightHistoryMatch
	seen := map[string]bool{}
	for rows.Next() {
		var leadEmail, leadDomain string
		var match CampaignPreflightHistoryMatch
		if err := rows.Scan(&leadEmail, &leadDomain, &match.LeadGlobalStatus,
			&match.CampaignID, &match.CampaignName, &match.WorkspaceID,
			&match.CampaignStatus, &match.CampaignLead); err != nil {
			return nil, err
		}
		leadEmail = normalizePreflightEmail(leadEmail)
		if leadEmail == email {
			match.MatchType = "email"
		} else if checkDomains && strings.EqualFold(strings.TrimSpace(leadDomain), domain) {
			match.MatchType = "domain"
		} else {
			continue
		}
		if historyScope == CampaignHistoryScopeWorkspace && match.CampaignID > 0 && match.WorkspaceID != workspaceID {
			continue
		}
		if historyScope == CampaignHistoryScopeWorkspace && match.CampaignID == 0 && match.LeadGlobalStatus == "active" {
			continue
		}
		match.LeadEmail = leadEmail
		key := fmt.Sprintf("%s|%s|%d|%s", match.MatchType, match.LeadEmail, match.CampaignID, match.LeadGlobalStatus)
		if seen[key] {
			continue
		}
		seen[key] = true
		matches = append(matches, match)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].MatchType != matches[j].MatchType {
			return matches[i].MatchType < matches[j].MatchType
		}
		if matches[i].LeadEmail != matches[j].LeadEmail {
			return matches[i].LeadEmail < matches[j].LeadEmail
		}
		return matches[i].CampaignID < matches[j].CampaignID
	})
	return matches, nil
}

func normalizePreflightEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
