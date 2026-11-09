// Package summary 实现按话题组织的日摘要流水线与确定性校验。
package summary

const (
	// RollingEvery 与 RollingMaxPerDay 由写事务触发滚动任务时使用。
	RollingEvery     = 200
	RollingMaxPerDay = 6
	sliceGapMinutes  = 30
	sliceMaxMsgs     = 200
	sliceMaxChars    = 8000
	sliceMsgMaxChars = sliceMaxChars / 4
	sliceOverlapMsgs = 10
	jobMaxSlices     = 80
	sliceConcurrency = 4
	llmTemperature   = 0.2
)

const fillerWords = "|嗯|嗯嗯|哦|噢|啊|哈|呵|草|艹|好|好的|行|对|ok|1|"

const (
	tSummary   = 3.0
	scoreAlpha = 1.0
	scoreTheta = 0.25
)

// WindowDays 是日摘要延续判断与自动上下文共同使用的日期窗口。
const WindowDays = 3
