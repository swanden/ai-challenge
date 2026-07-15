package main

import "github.com/anthropics/anthropic-sdk-go"

const (
	opusInPerM   = 5.0
	opusOutPerM  = 25.0
	haikuInPerM  = 1.0
	haikuOutPerM = 5.0
)

func priceFor(model anthropic.Model) (inPerM, outPerM float64) {
	if containsFold(string(model), "haiku") {
		return haikuInPerM, haikuOutPerM
	}
	return opusInPerM, opusOutPerM
}

func costFor(model anthropic.Model, input, output int64) float64 {
	in, out := priceFor(model)
	return float64(input)/1e6*in + float64(output)/1e6*out
}

func containsFold(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

type storedMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
