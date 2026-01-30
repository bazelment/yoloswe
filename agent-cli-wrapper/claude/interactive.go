package claude

import (
	"context"
	"fmt"
)

// InteractiveToolHandler handles tools that require user interaction.
// These tools are semantically different from permission-controlled tools -
// they need user input, not safety approval.
type InteractiveToolHandler interface {
	// HandleAskUserQuestion is called when Claude asks the user questions.
	// Returns a map of question text -> user's answer.
	// Return error to deny the tool.
	HandleAskUserQuestion(ctx context.Context, questions []Question) (map[string]string, error)

	// HandleExitPlanMode is called when Claude finishes planning and awaits approval.
	// Returns feedback to send back to Claude (approval message or refinement request).
	// Return error to deny the tool.
	HandleExitPlanMode(ctx context.Context, plan PlanInfo) (string, error)
}

// Question represents a question from AskUserQuestion tool.
type Question struct {
	Text        string           // The question text
	Options     []QuestionOption // Available options (may be empty for free-form)
	MultiSelect bool             // Whether multiple options can be selected
}

// QuestionOption represents an option for a question.
type QuestionOption struct {
	Label string // Option label (what gets selected)
}

// PlanInfo contains the plan details from ExitPlanMode.
type PlanInfo struct {
	Plan           string          // The plan content (read from plan file)
	AllowedPrompts []AllowedPrompt // Permissions requested for implementation
}

// AllowedPrompt represents a permission requested by the plan.
type AllowedPrompt struct {
	Tool   string // Tool name (e.g., "Bash")
	Prompt string // Description of allowed action
}

// ParseQuestionsFromInput extracts Question structs from raw tool input.
func ParseQuestionsFromInput(input map[string]interface{}) ([]Question, error) {
	questionsRaw, ok := input["questions"]
	if !ok {
		return nil, fmt.Errorf("missing 'questions' field in input")
	}

	questionsArr, ok := questionsRaw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("'questions' field is not an array")
	}

	questions := make([]Question, 0, len(questionsArr))
	for i, qRaw := range questionsArr {
		qMap, ok := qRaw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("question %d is not an object", i)
		}

		q := Question{}

		// Parse question text
		if text, ok := qMap["question"].(string); ok {
			q.Text = text
		} else {
			return nil, fmt.Errorf("question %d missing 'question' text", i)
		}

		// Parse options
		if optionsRaw, ok := qMap["options"]; ok {
			optionsArr, ok := optionsRaw.([]interface{})
			if !ok {
				return nil, fmt.Errorf("question %d 'options' is not an array", i)
			}

			q.Options = make([]QuestionOption, 0, len(optionsArr))
			for _, optRaw := range optionsArr {
				switch opt := optRaw.(type) {
				case string:
					q.Options = append(q.Options, QuestionOption{Label: opt})
				case map[string]interface{}:
					// Handle object-style options if needed
					if label, ok := opt["label"].(string); ok {
						q.Options = append(q.Options, QuestionOption{Label: label})
					}
				}
			}
		}

		// Parse multiSelect
		if multiSelect, ok := qMap["multiSelect"].(bool); ok {
			q.MultiSelect = multiSelect
		}

		questions = append(questions, q)
	}

	return questions, nil
}

// ParsePlanInfoFromInput extracts PlanInfo from raw tool input.
func ParsePlanInfoFromInput(input map[string]interface{}) (PlanInfo, error) {
	info := PlanInfo{}

	// Note: ExitPlanMode tool input typically doesn't contain the plan text directly.
	// The plan is stored in a file specified in the system prompt.
	// We parse what's available in the input.

	// Parse allowedPrompts if present
	if promptsRaw, ok := input["allowedPrompts"]; ok {
		promptsArr, ok := promptsRaw.([]interface{})
		if !ok {
			return info, fmt.Errorf("'allowedPrompts' is not an array")
		}

		info.AllowedPrompts = make([]AllowedPrompt, 0, len(promptsArr))
		for _, pRaw := range promptsArr {
			pMap, ok := pRaw.(map[string]interface{})
			if !ok {
				continue
			}

			prompt := AllowedPrompt{}
			if tool, ok := pMap["tool"].(string); ok {
				prompt.Tool = tool
			}
			if promptText, ok := pMap["prompt"].(string); ok {
				prompt.Prompt = promptText
			}
			info.AllowedPrompts = append(info.AllowedPrompts, prompt)
		}
	}

	return info, nil
}
