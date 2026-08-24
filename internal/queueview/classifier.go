package queueview

import "strings"

//go:generate go run ./cmd/classifierdoc -path ../../docs/specs/worker-profiles.md

type ClassificationMatch string

const (
	MatchSuffix   ClassificationMatch = "suffix"
	MatchExact    ClassificationMatch = "exact"
	MatchFallback ClassificationMatch = "fallback"
)

type ClassificationRule struct {
	Match      ClassificationMatch
	Suffix     string
	ExactKinds []string
	Role       Role
}

var classificationRules = []ClassificationRule{
	{Match: MatchSuffix, Suffix: "-discovery", Role: RoleDiscoverer},
	{Match: MatchExact, ExactKinds: []string{"pr-review"}, Role: RoleReviewer},
	{Match: MatchExact, ExactKinds: []string{"release-needed"}, Role: RoleUnassigned},
	{Match: MatchFallback, Role: RoleImplementer},
}

func ClassificationRules() []ClassificationRule {
	rules := make([]ClassificationRule, len(classificationRules))
	for index, rule := range classificationRules {
		rules[index] = rule
		rules[index].ExactKinds = append([]string(nil), rule.ExactKinds...)
	}
	return rules
}

func ClassificationRuleFor(role Role) (ClassificationRule, bool) {
	for _, rule := range classificationRules {
		if rule.Role == role {
			rule.ExactKinds = append([]string(nil), rule.ExactKinds...)
			return rule, true
		}
	}
	return ClassificationRule{}, false
}

func RoleSelection(role Role) string {
	rule, ok := ClassificationRuleFor(role)
	if !ok {
		return ""
	}
	switch rule.Match {
	case MatchSuffix:
		return "kinds ending in " + rule.Suffix + " only"
	case MatchExact:
		if len(rule.ExactKinds) == 1 {
			return "exact " + rule.ExactKinds[0] + " only"
		}
		return "exact " + strings.Join(rule.ExactKinds, ", ") + " only"
	case MatchFallback:
		return "every other kind after ordered classifier exclusions"
	default:
		return ""
	}
}

func Classify(kind string) Role {
	for _, rule := range classificationRules {
		switch rule.Match {
		case MatchSuffix:
			if strings.HasSuffix(kind, rule.Suffix) {
				return rule.Role
			}
		case MatchExact:
			for _, exact := range rule.ExactKinds {
				if kind == exact {
					return rule.Role
				}
			}
		case MatchFallback:
			return rule.Role
		}
	}
	return RoleUnassigned
}
