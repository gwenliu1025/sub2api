package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 同一个输出项从新增、完成到最终响应必须保持身份，工具call_id不替代item ID。
func TestChatResponsesStreamPreservesOutputIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*ChatCompletionsToResponsesStreamState)
		chunk string
	}{
		{name: "function", chunk: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"add","arguments":"{\"a\":1}"}}]}}]}`},
		{name: "custom", setup: func(s *ChatCompletionsToResponsesStreamState) { s.CustomTools = map[string]bool{"exec": true} }, chunk: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"exec","arguments":"{\"input\":\"hello\"}"}}]}}]}`},
		{name: "tool_search", setup: func(s *ChatCompletionsToResponsesStreamState) { s.ToolSearchDeclared = true }, chunk: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"tool_search","arguments":"{\"query\":\"lookup\"}"}}]}}]}`},
		{name: "reasoning_and_message", chunk: `{"choices":[{"delta":{"reasoning_content":"think","content":"answer"}}]}`},
		{name: "parallel_functions", chunk: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"first","arguments":"{}"}},{"index":1,"id":"call_2","function":{"name":"second","arguments":"{}"}}]}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := NewChatCompletionsToResponsesStreamState("glm-5.3")
			if tc.setup != nil {
				tc.setup(state)
			}
			var chunk ChatCompletionsChunk
			require.NoError(t, json.Unmarshal([]byte(tc.chunk), &chunk))
			events := ChatCompletionsChunkToResponsesEvents(&chunk, state)
			events = append(events, FinalizeChatCompletionsResponsesStream(state)...)
			added := map[int]ResponsesOutput{}
			var completed *ResponsesResponse
			for _, event := range events {
				switch event.Type {
				case "response.output_item.added":
					require.NotNil(t, event.Item)
					added[event.OutputIndex] = *event.Item
				case "response.output_item.done":
					require.NotNil(t, event.Item)
					require.Equal(t, added[event.OutputIndex].ID, event.Item.ID)
				case "response.completed":
					completed = event.Response
				}
			}
			require.NotNil(t, completed)
			require.NotEmpty(t, added)
			require.Len(t, completed.Output, len(added))
			for index, item := range completed.Output {
				require.NotEmpty(t, item.ID)
				require.Equal(t, added[index].ID, item.ID, "最终聚合不得给同一输出项重新编号")
				require.Equal(t, added[index].CallID, item.CallID)
			}
			require.Empty(t, FinalizeChatCompletionsResponsesStream(state), "重复收尾不得重发事件")
		})
	}
}
