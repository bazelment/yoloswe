package render

// QuestionOption represents an option for a question.
type QuestionOption struct {
	Label       string
	Description string
}

// ParseQuestionOptions parses options from the AskUserQuestion input.
// Options can be strings or objects with label/description fields.
func ParseQuestionOptions(optionsRaw []interface{}) []QuestionOption {
	options := make([]QuestionOption, 0, len(optionsRaw))
	for _, opt := range optionsRaw {
		switch o := opt.(type) {
		case string:
			options = append(options, QuestionOption{Label: o})
		case map[string]interface{}:
			label, _ := o["label"].(string)
			desc, _ := o["description"].(string)
			if label != "" {
				options = append(options, QuestionOption{Label: label, Description: desc})
			}
		}
	}
	return options
}
