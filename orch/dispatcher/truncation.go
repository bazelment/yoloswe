package dispatcher

import (
	"context"
	"strings"

	"github.com/bazelment/yoloswe/orch/model"
)

// maxTruncationRetries bounds how many times a turn cut off at the output-token
// limit is re-asked before the cut-off response is returned as is.
const maxTruncationRetries = 2

// truncationNudge tells the model its previous reply hit the output limit.
const truncationNudge = "Your previous reply was cut off by the output limit before any tool call completed, so nothing was decided. Call the tool now with no preamble, then end with one short sentence."

// retryOnTruncationClient re-asks a turn that stopped at max_tokens. A reply
// that spends its whole output budget on analysis (or on a half-written tool
// call) decides nothing, and the batch is then lost: the tool loop treats any
// stop other than tool_use as the end of the turn.
type retryOnTruncationClient struct {
	inner model.ModelClient
}

func (c retryOnTruncationClient) CreateMessage(ctx context.Context, req model.Request) (*model.Response, error) {
	var usage model.Usage
	cur := req
	for attempt := 0; ; attempt++ {
		resp, err := c.inner.CreateMessage(ctx, cur)
		if err != nil || resp == nil {
			return resp, err
		}
		usage = usage.Add(resp.Usage)
		if resp.StopReason != model.StopReasonMaxTokens || attempt >= maxTruncationRetries {
			out := resp.Clone()
			out.Usage = usage
			return &out, nil
		}
		cur = req.Clone()
		if prose := strings.TrimSpace(resp.Text()); prose != "" {
			cur.Messages = append(cur.Messages, model.Message{
				Role:    model.RoleAssistant,
				Content: []model.ContentBlock{{Type: model.BlockTypeText, Text: prose}},
			})
		} else {
			// The user turn must still alternate with an assistant turn.
			cur.Messages = append(cur.Messages, model.Message{
				Role:    model.RoleAssistant,
				Content: []model.ContentBlock{{Type: model.BlockTypeText, Text: "(cut off)"}},
			})
		}
		cur.Messages = append(cur.Messages, model.NewUserMessage(truncationNudge))
	}
}
