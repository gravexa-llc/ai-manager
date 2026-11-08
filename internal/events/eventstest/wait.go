package eventstest

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/events"
)

// WaitFor is the wait detail the mod sends with a needs_you about a call
// of tool with input (internal/events Wait), or nil when it
// has nothing to say. It is plugin/mod/wait.ts written in Go: both follow
// the same rules, and both test them on the same examples.
//
//	AskUserQuestion   question: each question (at most 4) with its header and
//	                  option labels (at most 6 of 100 characters), multi_select
//	ExitPlanMode      plan: the plan's first 12 lines, 1000 characters at most
//	                  (nil when the call carries no plan)
//	anything else     permission: the tool and Summary(tool, input)
func WaitFor(tool string, input map[string]any) *events.Wait {
	switch tool {
	case "AskUserQuestion":
		list, _ := input["questions"].([]any)
		var qs []events.Question
		for _, item := range list {
			if len(qs) == events.MaxWaitQuestions {
				break
			}
			m, _ := item.(map[string]any)
			q := events.Question{
				Header:   events.RedactLine(str(m, "header"), events.MaxKindLen),
				Question: events.RedactLine(str(m, "question"), events.MaxSummaryLen),
			}
			q.MultiSelect, _ = m["multiSelect"].(bool)
			opts, _ := m["options"].([]any)
			for _, o := range opts {
				om, _ := o.(map[string]any)
				label := events.RedactLine(str(om, "label"), events.MaxOptionLen)
				if label != "" && len(q.Options) < events.MaxWaitOptions {
					q.Options = append(q.Options, label)
				}
			}
			if q.Question != "" || len(q.Options) > 0 {
				qs = append(qs, q)
			}
		}
		if len(qs) == 0 {
			return nil
		}
		return &events.Wait{Kind: events.WaitQuestion, Tool: tool, Questions: qs}
	case "ExitPlanMode":
		plan := events.PlanLines(events.Redact(events.PlanLines(str(input, "plan"))))
		if plan == "" {
			return nil
		}
		return &events.Wait{Kind: events.WaitPlan, Tool: tool, Plan: plan}
	}
	if tool == "" {
		return nil
	}
	return &events.Wait{Kind: events.WaitPermission, Tool: events.Truncate(tool, events.MaxToolLen), Summary: Summary(tool, input)}
}

// Summary is a permission's one-line summary of what the call does: more
// than its label (the whole Bash command, the full path), never its
// output, at most 300 characters, with the secrets events.Redact spots replaced.
//
//	Bash                                 the command
//	Edit, MultiEdit, Write, Read,        the file path
//	NotebookEdit
//	WebFetch                             the URL
//	WebSearch                            the query
//	Agent, Task                          the description, else the agent type
//	Grep, Glob                           the pattern, and " in <path>" when given
//	SendMessage                          "to <to>"
//	EnterWorktree, ExitWorktree          the name, the action
//	Skill                                the skill
//	anything else (MCP tools, …)         "key: value" for each top-level string,
//	                                     number or boolean, keys sorted, values
//	                                     cut to 60 characters, joined by ", "
func Summary(tool string, input map[string]any) string {
	var s string
	switch tool {
	case "Bash":
		s = str(input, "command")
	case "Edit", "MultiEdit", "Write", "Read", "NotebookEdit":
		s = cmp.Or(str(input, "file_path"), str(input, "notebook_path"))
	case "WebFetch":
		s = str(input, "url")
	case "WebSearch":
		s = str(input, "query")
	case "Agent", "Task":
		s = cmp.Or(str(input, "description"), str(input, "subagent_type"))
	case "Grep", "Glob":
		s = str(input, "pattern")
		if p := str(input, "path"); p != "" && s != "" {
			s += " in " + p
		}
	case "SendMessage":
		if to := str(input, "to"); to != "" {
			s = "to " + to
		}
	case "EnterWorktree":
		s = str(input, "name")
	case "ExitWorktree":
		s = str(input, "action")
	case "Skill":
		s = str(input, "skill")
	default:
		keys := make([]string, 0, len(input))
		for k := range input {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var parts []string
		for _, k := range keys {
			var v string
			switch x := input[k].(type) {
			case string:
				v = events.Truncate(events.OneLine(x), 60)
			case bool:
				v = fmt.Sprint(x)
			case float64:
				v = fmt.Sprint(x)
			default:
				continue
			}
			parts = append(parts, k+": "+v)
		}
		s = strings.Join(parts, ", ")
	}
	// Cut first, so a long heredoc costs little, then redact and cut again.
	s = events.Truncate(events.OneLine(s), 1000)
	return events.Truncate(events.OneLine(events.Redact(s)), events.MaxSummaryLen)
}
