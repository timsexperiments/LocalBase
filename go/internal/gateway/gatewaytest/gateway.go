package gatewaytest

import "io"

// ChatModel is the chat model of the test catalog.
const ChatModel = "qwen3.5-9b-q4_k_m"

// Log, when set, receives the next test gateway's JSON log.
var Log io.Writer
