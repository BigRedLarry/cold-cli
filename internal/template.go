package internal

import (
	"regexp"
	"strings"
)

var placeholderRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// BuiltinFields are fields always available for template rendering from the leads table.
var BuiltinFields = []string{"email", "first_name", "last_name", "company", "domain"}

// SenderFields are derived from the account assigned to a scheduled send.
var SenderFields = []string{"sender_email", "sender_local", "sender_domain", "sender_name"}

var senderFieldSet = map[string]bool{
	"sender_email":  true,
	"sender_local":  true,
	"sender_domain": true,
	"sender_name":   true,
}

// fieldAliases maps common shorthand names to their canonical field names.
var fieldAliases = map[string]string{
	"name":      "first_name",
	"firstname": "first_name",
	"first":     "first_name",
	"lastname":  "last_name",
	"last":      "last_name",
}

// ResolveAlias returns the canonical field name for a known alias, or the name unchanged.
func ResolveAlias(name string) string {
	if canonical, ok := fieldAliases[name]; ok {
		return canonical
	}
	return name
}

// ExtractPlaceholders returns all unique {{placeholder}} names from a string.
func ExtractPlaceholders(s string) []string {
	matches := placeholderRe.FindAllStringSubmatch(s, -1)
	seen := map[string]bool{}
	var result []string
	for _, m := range matches {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	return result
}

// RenderTemplate replaces all {{placeholder}} occurrences with values from the fields map.
// It also resolves known aliases (e.g., {{name}} → first_name value).
func RenderTemplate(tmpl string, fields map[string]string) string {
	result := tmpl
	for key, val := range fields {
		result = strings.ReplaceAll(result, "{{"+key+"}}", val)
	}
	// Replace alias patterns that map to existing field values
	for alias, canonical := range fieldAliases {
		if _, hasAlias := fields[alias]; hasAlias {
			continue // already replaced above
		}
		if val, ok := fields[canonical]; ok {
			result = strings.ReplaceAll(result, "{{"+alias+"}}", val)
		}
	}
	return result
}

// SenderTemplateFields returns template fields derived from a sending account.
func SenderTemplateFields(fromEmail string) map[string]string {
	fields := map[string]string{
		"sender_email": fromEmail,
	}

	local, domain, ok := strings.Cut(fromEmail, "@")
	if ok {
		fields["sender_local"] = local
		fields["sender_domain"] = domain
	} else {
		fields["sender_local"] = fromEmail
		fields["sender_domain"] = ""
	}
	fields["sender_name"] = senderNameFromLocal(fields["sender_local"])

	return fields
}

func senderNameFromLocal(local string) string {
	base, _, _ := strings.Cut(local, "+")
	parts := strings.FieldsFunc(base, func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	})
	if len(parts) == 0 {
		return ""
	}

	for i, part := range parts {
		parts[i] = titleASCII(part)
	}
	return strings.Join(parts, " ")
}

func titleASCII(value string) string {
	value = strings.ToLower(value)
	if value == "" {
		return ""
	}
	b := []byte(value)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

// doubleSpaceRe matches two or more consecutive spaces.
var doubleSpaceRe = regexp.MustCompile(`  +`)

// StripUnresolved removes any remaining {{...}} placeholders from a string,
// collapses double spaces left behind, and returns the names of stripped variables.
func StripUnresolved(s string) (string, []string) {
	matches := placeholderRe.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return s, nil
	}

	var stripped []string
	seen := map[string]bool{}
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			stripped = append(stripped, m[1])
		}
	}

	result := placeholderRe.ReplaceAllString(s, "")
	result = doubleSpaceRe.ReplaceAllString(result, " ")
	result = strings.TrimSpace(result)
	return result, stripped
}

// levenshteinDistance computes the edit distance between two strings.
func levenshteinDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)

	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			best := del
			if ins < best {
				best = ins
			}
			if sub < best {
				best = sub
			}
			curr[j] = best
		}
		prev, curr = curr, prev
	}

	return prev[lb]
}

// SuggestField returns the closest matching field name if within Levenshtein distance 3, or "".
func SuggestField(name string, available []string) string {
	bestDist := 4 // only suggest if distance <= 3
	bestMatch := ""
	for _, f := range available {
		d := levenshteinDistance(name, f)
		if d < bestDist {
			bestDist = d
			bestMatch = f
		}
	}
	return bestMatch
}
