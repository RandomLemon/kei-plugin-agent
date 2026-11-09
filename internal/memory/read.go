package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/summary"
)

// Context 只读取可信 Scene 指定的会话，返回预算内的近期日摘要。
func (s *Service) Context(ctx context.Context, req ContextRequest) (ContextResult, error) {
	s.mu.RLock()
	db, ready := s.db, s.state == StateReady
	s.mu.RUnlock()
	if !ready || db == nil {
		return ContextResult{}, ErrNotReady
	}
	key, ok := convKey(req.Scene.Conv)
	if !ok {
		return ContextResult{}, fmt.Errorf("memory: 会话身份不完整")
	}
	today := store.LocalDate(s.now(), s.cfg.Location, s.dayStart)
	var candidates []summary.Candidate
	err := db.WithContext(ctx).Table("mem_topic AS t").Select("t.*, s.overview, s.kind").Joins("JOIN mem_summary AS s ON s.id = t.summary_id").Where("t.conv_key = ? AND s.conv_key = ? AND t.local_date >= ? AND t.local_date <= ? AND ((s.kind = 'daily' AND t.local_date < ?) OR (s.kind = 'rolling' AND NOT EXISTS (SELECT 1 FROM mem_summary d WHERE d.conv_key=s.conv_key AND d.local_date=s.local_date AND d.kind='daily')))", key, key, today.AddDate(0, 0, -summary.WindowDays), today, today).Order("t.local_date DESC, t.id").Scan(&candidates).Error
	if err != nil {
		s.mu.RLock()
		stopped := s.stopped
		s.mu.RUnlock()
		if stopped {
			return ContextResult{}, ErrNotReady
		}
		return ContextResult{}, err
	}
	lines := summary.SelectLines(candidates, req.Queries, today, req.BudgetChars, req.HistorySince)
	if len(lines) == 0 {
		return ContextResult{}, nil
	}
	return ContextResult{Blocks: []Block{{Title: "近期重点", Lines: lines}}}, nil
}

// QuerySummaries 按日期和关键词查询可信 Scene 指定的会话，不受自动上下文窗口限制。
func (s *Service) QuerySummaries(ctx context.Context, req SummaryQuery) ([]SummaryItem, error) {
	s.mu.RLock()
	db, ready := s.db, s.state == StateReady
	s.mu.RUnlock()
	if !ready || db == nil {
		return nil, ErrNotReady
	}
	key, ok := convKey(req.Scene.Conv)
	if !ok {
		return nil, fmt.Errorf("memory: 会话身份不完整")
	}
	today := store.LocalDate(s.now(), s.cfg.Location, s.dayStart)
	from, to, err := summaryDateRange(req.DateFrom, req.DateTo, today)
	if err != nil {
		return nil, err
	}
	var candidates []summary.Candidate
	err = db.WithContext(ctx).Table("mem_topic AS t").Select("t.*, s.kind").Joins("JOIN mem_summary AS s ON s.id=t.summary_id").Where("t.conv_key=? AND s.conv_key=? AND t.local_date BETWEEN ? AND ? AND (s.kind='daily' OR (s.kind='rolling' AND NOT EXISTS (SELECT 1 FROM mem_summary d WHERE d.conv_key=s.conv_key AND d.local_date=s.local_date AND d.kind='daily')))", key, key, from, to).Order("t.local_date DESC, t.importance DESC, t.id").Scan(&candidates).Error
	if err != nil {
		s.mu.RLock()
		stopped := s.stopped
		s.mu.RUnlock()
		if stopped {
			return nil, ErrNotReady
		}
		return nil, err
	}
	type ranked struct {
		candidate summary.Candidate
		relevance float64
	}
	topics := make([]ranked, 0, len(candidates))
	anyRelevant := false
	for _, c := range candidates {
		r := summary.KeywordRelevance(req.Query, c.Title+c.Summary)
		anyRelevant = anyRelevant || r > 0
		topics = append(topics, ranked{c, r})
	}
	slices.SortStableFunc(topics, func(a, b ranked) int {
		if anyRelevant {
			if c := cmp.Compare(b.relevance, a.relevance); c != 0 {
				return c
			}
		}
		return cmp.Or(b.candidate.LocalDate.Compare(a.candidate.LocalDate), cmp.Compare(b.candidate.Importance, a.candidate.Importance))
	})
	limit := req.Limit
	if limit == 0 {
		limit = 8
	}
	limit = max(1, min(10, limit))
	var items []SummaryItem
	for _, t := range topics {
		if anyRelevant && t.relevance == 0 {
			continue
		}
		c := t.candidate
		items = append(items, SummaryItem{Date: c.LocalDate.Format("2006-01-02"), Kind: c.Kind, Title: c.Title, Summary: c.Summary, OpenItems: summary.OpenItems(c.KeyPoints)})
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

func summaryDateRange(dateFrom, dateTo string, today time.Time) (time.Time, time.Time, error) {
	var from, to time.Time
	var err error
	if dateFrom != "" {
		from, err = time.Parse("2006-01-02", dateFrom)
		if err != nil {
			return from, to, ErrInvalidDateRange
		}
	}
	if dateTo != "" {
		to, err = time.Parse("2006-01-02", dateTo)
		if err != nil {
			return from, to, ErrInvalidDateRange
		}
	}
	switch {
	case dateFrom == "" && dateTo == "":
		from, to = today.AddDate(0, 0, -6), today
	case dateFrom == "":
		from = to.AddDate(0, 0, -6)
	case dateTo == "":
		to = from.AddDate(0, 0, 30)
		if to.After(today) {
			to = today
		}
	}
	if from.After(to) || to.Sub(from) > 30*24*time.Hour {
		return from, to, ErrInvalidDateRange
	}
	return from, to, nil
}
