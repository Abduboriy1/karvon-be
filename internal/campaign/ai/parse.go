package ai

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
)

// Size limits on generated content, matching the schema's columns.
const (
	MaxNameLen = 120
	MaxBodyLen = 5000
	MaxTagLen  = 40
)

// rawVariant mirrors VariantRef with a nullable subject so a missing index is
// told apart from index 0.
type rawVariant struct {
	Name      string `json:"name"`
	Subject   *int   `json:"subject"`
	Hook      *int   `json:"hook"`
	Problem   *int   `json:"problem"`
	ValueProp *int   `json:"value_prop"`
	Proof     *int   `json:"proof"`
	CTA       *int   `json:"cta"`
	Closing   *int   `json:"closing"`
	PS        *int   `json:"ps"`
}

type rawOutput struct {
	Components []Component  `json:"components"`
	Variants   []rawVariant `json:"variants"`
}

// ParseOutput decodes and validates what the model (or the operator's paste)
// produced. It is tolerant of the usual noise: markdown fences and prose around
// the JSON object are stripped. Hard failures come back as apperr.Validation with
// one FieldError per problem; soft ones (a variant pointing at the wrong type, a
// duplicate name) are repaired and reported in Output.Warnings.
func ParseOutput(raw string) (Output, error) {
	body, ok := extractJSON(raw)
	if !ok {
		return Output{}, apperr.Validation("no JSON object found in the pasted output",
			apperr.FieldError{Field: "raw", Message: "paste the whole JSON reply, starting with { and ending with }"})
	}

	var in rawOutput
	dec := json.NewDecoder(strings.NewReader(body))
	if err := dec.Decode(&in); err != nil {
		return Output{}, apperr.Validation("the output is not valid JSON",
			apperr.FieldError{Field: "raw", Message: err.Error()}).WithCause(err)
	}

	var fields []apperr.FieldError
	out := Output{Components: make([]Component, 0, len(in.Components))}

	used := make(map[string]bool) // lower-cased names already taken
	for i, c := range in.Components {
		prefix := "components[" + strconv.Itoa(i) + "]"
		c.Type = strings.TrimSpace(c.Type)
		c.Name = strings.TrimSpace(c.Name)
		c.Body = strings.TrimSpace(c.Body)
		c.Tags = cleanTags(c.Tags)

		switch {
		case !campaign.ValidComponentType(c.Type):
			fields = append(fields, apperr.FieldError{Field: prefix + ".type",
				Message: fmt.Sprintf("unknown component type %q", c.Type)})
		case c.Name == "":
			fields = append(fields, apperr.FieldError{Field: prefix + ".name", Message: "name is required"})
		case len(c.Name) > MaxNameLen:
			fields = append(fields, apperr.FieldError{Field: prefix + ".name",
				Message: fmt.Sprintf("name is longer than %d characters", MaxNameLen)})
		case c.Body == "":
			fields = append(fields, apperr.FieldError{Field: prefix + ".body", Message: "body is required"})
		case len(c.Body) > MaxBodyLen:
			fields = append(fields, apperr.FieldError{Field: prefix + ".body",
				Message: fmt.Sprintf("body is longer than %d characters", MaxBodyLen)})
		}

		// Names are what the operator sees in lists, so keep them unique.
		if c.Name != "" && used[strings.ToLower(c.Name)] {
			original := c.Name
			for n := 2; ; n++ {
				c.Name = original + " " + strconv.Itoa(n)
				if !used[strings.ToLower(c.Name)] {
					break
				}
			}
			out.Warnings = append(out.Warnings,
				fmt.Sprintf("component %d: duplicate name %q renamed to %q", i, original, c.Name))
		}
		used[strings.ToLower(c.Name)] = true
		out.Components = append(out.Components, c)
	}

	if len(fields) > 0 {
		return Output{}, apperr.Validation("generated content failed validation", fields...)
	}
	if len(out.Components) == 0 {
		return Output{}, apperr.Validation("generated content has no components",
			apperr.FieldError{Field: "components", Message: "at least one component is required"})
	}

	out.Variants = make([]VariantRef, 0, len(in.Variants))
	for i, v := range in.Variants {
		ref, problem := out.variant(v)
		if problem != "" {
			out.Warnings = append(out.Warnings, fmt.Sprintf("variant %d dropped: %s", i, problem))
			continue
		}
		if ref.Name = strings.TrimSpace(ref.Name); ref.Name == "" {
			ref.Name = "Variant " + strconv.Itoa(len(out.Variants)+1)
		}
		// Truncate on characters, not bytes: slicing bytes can split a multi-byte
		// rune and store invalid UTF-8.
		if runes := []rune(ref.Name); len(runes) > MaxNameLen {
			ref.Name = string(runes[:MaxNameLen])
		}
		out.Variants = append(out.Variants, ref)
	}
	return out, nil
}

// variant checks every index of a raw variant against the component list and
// says what is wrong with the first bad one.
func (o *Output) variant(v rawVariant) (VariantRef, string) {
	ref := VariantRef{Name: v.Name}
	if v.Subject == nil {
		return ref, "no subject index"
	}
	if p := o.check(*v.Subject, campaign.ComponentSubject); p != "" {
		return ref, p
	}
	ref.SubjectIndex = *v.Subject

	optional := []struct {
		typ string
		in  *int
		out **int
	}{
		{campaign.ComponentHook, v.Hook, &ref.HookIndex},
		{campaign.ComponentProblem, v.Problem, &ref.ProblemIndex},
		{campaign.ComponentValueProp, v.ValueProp, &ref.ValuePropIndex},
		{campaign.ComponentProof, v.Proof, &ref.ProofIndex},
		{campaign.ComponentCTA, v.CTA, &ref.CTAIndex},
		{campaign.ComponentClosing, v.Closing, &ref.ClosingIndex},
		{campaign.ComponentPS, v.PS, &ref.PSIndex},
	}
	for _, part := range optional {
		if part.in == nil {
			continue
		}
		if p := o.check(*part.in, part.typ); p != "" {
			return ref, p
		}
		idx := *part.in
		*part.out = &idx
	}
	return ref, ""
}

// check reports why an index is not a usable pointer at a component of a type.
func (o *Output) check(idx int, typ string) string {
	if idx < 0 || idx >= len(o.Components) {
		return fmt.Sprintf("%s index %d is out of range", typ, idx)
	}
	if got := o.Components[idx].Type; got != typ {
		return fmt.Sprintf("%s index %d points at a %s component", typ, idx, got)
	}
	return ""
}

// extractJSON returns the outermost {...} in the text, ignoring fences and prose.
func extractJSON(raw string) (string, bool) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return "", false
	}
	return raw[start : end+1], true
}

// cleanTags trims, lower-cases, de-duplicates and drops empty or oversized tags.
func cleanTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || len(t) > MaxTagLen || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
