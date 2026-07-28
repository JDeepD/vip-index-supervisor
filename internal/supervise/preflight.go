package supervise

import (
	"context"
	"fmt"
	"regexp"
)

// Only `post` is registered unconditionally. The others exist only while the
// ElasticPress feature that owns them is active, which is what "Indexable X
// not found. Is the feature active?" is really telling you.
var featureForIndexable = map[string]string{
	"term":    "terms",
	"user":    "users",
	"comment": "comments",
}

var reIndexableMissing = regexp.MustCompile(`(?i)Indexable\s+\w+\s+not found`)

// preflight finds problems that would abort a phase, before any work is done.
// Every indexable is checked up front so a misconfiguration surfaces
// immediately rather than hours into a multi-phase run, after earlier phases
// have already committed their index versions.
func (s *Supervisor) preflight(ctx context.Context) []string {
	var problems []string
	for _, indexable := range s.cfg.Indexables {
		if len(s.client.Versions(ctx, indexable)) > 0 {
			continue
		}
		if reIndexableMissing.MatchString(s.client.LastRun.Output) {
			msg := fmt.Sprintf("'%s' is not a registered indexable", indexable)
			if feature := featureForIndexable[indexable]; feature != "" {
				msg += fmt.Sprintf(" — enable it first: %s activate-feature %s",
					s.commandHint(), feature)
			}
			problems = append(problems, msg)
			continue
		}
		lines := s.client.LastRun.DescribeFailure()
		problems = append(problems, fmt.Sprintf(
			"could not read index versions for '%s': %s", indexable, lines[len(lines)-1]))
	}
	return problems
}
