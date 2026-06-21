package postgresql

import (
	"fmt"
	"strings"
)

const (
	queryConcatKeywordWhere = "WHERE"
	queryConcatKeywordAnd   = "AND"
	queryArrayKeywordAny    = "ANY"
	queryArrayKeywordAll    = "ALL"
	likePatternQuery        = "LIKE"
	notLikePatternQuery     = "NOT LIKE"
	regexPatternQuery       = "~"
)

func generatePatternMatchingString(patternMatchingTarget string, additionalQueryKeyword string, pattern string) string {
	patternMatchingFilter := fmt.Sprintf("%s %s %s", patternMatchingTarget, additionalQueryKeyword, pattern)

	return patternMatchingFilter
}

func applyTypeMatchingToQuery(objectKeyword string, objects []any) string {
	var typeFilter string
	if len(objects) > 0 {
		typeFilter = fmt.Sprintf("%s = %s", objectKeyword, generatePatternArrayString(objects, queryArrayKeywordAny))
	}

	return typeFilter
}

func generatePatternArrayString(patterns []any, queryArrayKeyword string) string {
	formattedPatterns := []string{}

	for _, pattern := range patterns {
		formattedPatterns = append(formattedPatterns, fmt.Sprintf("'%s'", pattern.(string)))
	}
	return fmt.Sprintf("%s (array[%s])", queryArrayKeyword, strings.Join(formattedPatterns, ","))
}

func finalizeQueryWithFilters(query string, queryConcatKeyword string, filters []string) string {
	if len(filters) > 0 {
		query = fmt.Sprintf("%s %s %s", query, queryConcatKeyword, strings.Join(filters, " AND "))
	}

	return query
}
