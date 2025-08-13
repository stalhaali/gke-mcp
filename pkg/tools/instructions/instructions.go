// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package instructions

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/GoogleCloudPlatform/gke-mcp/pkg/config"
	"github.com/GoogleCloudPlatform/gke-mcp/pkg/install"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type InstructionsRAG struct {
	sections []Section
}

type Section struct {
	Title   string
	Content string
	Level   int
}

type ScoredSection struct {
	Section
	Score float64
}

func Install(_ context.Context, s *server.MCPServer, _ *config.Config) error {
	rag := NewInstructionsRAG()

	getInstructionsTool := mcp.NewTool("get_instructions",
		mcp.WithDescription("Retrieve specific instructions from the GKE MCP server documentation. ONLY use this tool when the user explicitly requests GKE MCP instructions by saying 'Using the GKE MCP Instructions', 'Use the GKE MCP Instructions', or similar phrases."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("query", mcp.Required(), mcp.Description("The user's question or topic after they've requested GKE MCP instructions")),
		mcp.WithNumber("max_sections", mcp.Description("Maximum number of relevant sections to return (default: 3, max: 10)")),
	)

	s.AddTool(getInstructionsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return rag.handleGetInstructions(ctx, request)
	})

	return nil
}

func NewInstructionsRAG() *InstructionsRAG {
	rag := &InstructionsRAG{}
	rag.indexInstructions()
	return rag
}

func (r *InstructionsRAG) indexInstructions() {
	content := string(install.GeminiMarkdown)
	r.sections = r.parseMarkdown(content)
}

func (r *InstructionsRAG) parseMarkdown(content string) []Section {
	lines := strings.Split(content, "\n")
	var sections []Section
	var currentSection Section
	var contentLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Check if this is a header
		if strings.HasPrefix(trimmed, "#") {
			// Save previous section if it exists
			if currentSection.Title != "" {
				currentSection.Content = strings.TrimSpace(strings.Join(contentLines, "\n"))
				if currentSection.Content != "" {
					sections = append(sections, currentSection)
				}
			}

			// Start new section
			level := 0
			for _, char := range trimmed {
				if char == '#' {
					level++
				} else {
					break
				}
			}

			currentSection = Section{
				Title: strings.TrimSpace(trimmed[level:]),
				Level: level,
			}
			contentLines = []string{}
		} else if currentSection.Title != "" {
			// Add content to current section
			contentLines = append(contentLines, line)
		}
	}

	// Don't forget the last section
	if currentSection.Title != "" {
		currentSection.Content = strings.TrimSpace(strings.Join(contentLines, "\n"))
		if currentSection.Content != "" {
			sections = append(sections, currentSection)
		}
	}

	return sections
}

func (r *InstructionsRAG) handleGetInstructions(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := request.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Check if the original request contains the trigger phrases
	// This is a safeguard - the AI should already be filtering based on the tool description
	queryLower := strings.ToLower(query)
	triggerPhrases := []string{
		"using the gke mcp instructions",
		"use the gke mcp instructions",
		"gke mcp instructions",
		"with gke mcp instructions",
		"from gke mcp instructions",
	}

	for _, phrase := range triggerPhrases {
		if strings.Contains(queryLower, phrase) {
			// Remove the trigger phrase from the query for better search results
			query = strings.ReplaceAll(queryLower, phrase, "")
			query = strings.TrimSpace(query)
			break
		}
	}

	// If query becomes empty after removing trigger phrase, provide guidance
	if query == "" {
		return mcp.NewToolResultText("Please specify what aspect of GKE you need instructions for (e.g., 'logging', 'cost analysis', 'authentication', 'cluster management')."), nil
	}

	maxSections := 3 // default value
	if args, ok := request.Params.Arguments.(map[string]any); ok {
		if maxSectionsParam, exists := args["max_sections"]; exists {
			if maxSectionsFloat, ok := maxSectionsParam.(float64); ok {
				maxSections = int(maxSectionsFloat)
			}
		}
	}

	if maxSections > 10 {
		maxSections = 10
	} else if maxSections < 1 {
		maxSections = 1
	}

	relevantSections := r.findRelevantSections(query, maxSections)

	if len(relevantSections) == 0 {
		return mcp.NewToolResultText("No relevant instructions found for your query. You may want to try different keywords or check the full documentation."), nil
	}

	// Format the response
	var result strings.Builder
	result.WriteString(fmt.Sprintf("# Relevant GKE MCP Instructions for: \"%s\"\n\n", query))

	for i, scoredSection := range relevantSections {
		if i > 0 {
			result.WriteString("\n---\n\n")
		}

		// Add section title with appropriate header level
		headerPrefix := strings.Repeat("#", scoredSection.Level)
		result.WriteString(fmt.Sprintf("%s %s\n\n", headerPrefix, scoredSection.Title))

		// Add content
		result.WriteString(scoredSection.Content)
		result.WriteString("\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

func (r *InstructionsRAG) findRelevantSections(query string, maxSections int) []ScoredSection {
	query = strings.ToLower(query)
	queryTerms := r.tokenize(query)

	var scoredSections []ScoredSection

	for _, section := range r.sections {
		score := r.calculateRelevanceScore(queryTerms, section)
		if score > 0 {
			scoredSections = append(scoredSections, ScoredSection{
				Section: section,
				Score:   score,
			})
		}
	}

	// Sort by relevance score (descending)
	sort.Slice(scoredSections, func(i, j int) bool {
		return scoredSections[i].Score > scoredSections[j].Score
	})

	// Return top maxSections
	if len(scoredSections) > maxSections {
		scoredSections = scoredSections[:maxSections]
	}

	return scoredSections
}

func (r *InstructionsRAG) calculateRelevanceScore(queryTerms []string, section Section) float64 {
	if len(queryTerms) == 0 {
		return 0
	}

	titleText := strings.ToLower(section.Title)
	contentText := strings.ToLower(section.Content)
	allText := titleText + " " + contentText

	titleTerms := r.tokenize(titleText)
	contentTerms := r.tokenize(contentText)
	allTerms := append(titleTerms, contentTerms...)

	var score float64

	// Calculate TF-IDF-like scoring
	for _, queryTerm := range queryTerms {
		// Exact matches in title get highest weight
		if strings.Contains(titleText, queryTerm) {
			score += 10.0
		}

		// Partial matches in title
		for _, titleTerm := range titleTerms {
			if strings.Contains(titleTerm, queryTerm) || strings.Contains(queryTerm, titleTerm) {
				score += 5.0
			}
		}

		// Exact matches in content
		contentMatches := strings.Count(contentText, queryTerm)
		if contentMatches > 0 {
			// Logarithmic scaling to prevent single terms from dominating
			score += math.Log(float64(contentMatches)+1) * 2.0
		}

		// Partial matches in content
		for _, contentTerm := range contentTerms {
			if len(queryTerm) > 3 && len(contentTerm) > 3 {
				if strings.Contains(contentTerm, queryTerm) || strings.Contains(queryTerm, contentTerm) {
					score += 1.0
				}
			}
		}
	}

	// Boost score for certain high-value keywords
	highValueKeywords := map[string]float64{
		"log":           2.0,
		"logs":          2.0,
		"logging":       2.0,
		"query":         2.0,
		"cost":          2.0,
		"cluster":       1.5,
		"auth":          2.0,
		"authentication": 2.0,
		"giq":           2.0,
		"monitoring":    1.5,
		"kubectl":       1.5,
		"gcloud":        1.5,
	}

	for _, queryTerm := range queryTerms {
		if boost, exists := highValueKeywords[queryTerm]; exists {
			if strings.Contains(allText, queryTerm) {
				score *= boost
			}
		}
	}

	// Normalize by content length to prevent very long sections from always winning
	wordCount := float64(len(allTerms))
	if wordCount > 0 {
		score = score / math.Sqrt(wordCount) * 10.0
	}

	return score
}

func (r *InstructionsRAG) tokenize(text string) []string {
	// Simple tokenization - split by whitespace and punctuation
	var tokens []string
	var currentToken strings.Builder

	for _, char := range text {
		if unicode.IsLetter(char) || unicode.IsNumber(char) {
			currentToken.WriteRune(char)
		} else {
			if currentToken.Len() > 0 {
				token := currentToken.String()
				if len(token) > 1 { // Filter out single characters
					tokens = append(tokens, token)
				}
				currentToken.Reset()
			}
		}
	}

	// Don't forget the last token
	if currentToken.Len() > 0 {
		token := currentToken.String()
		if len(token) > 1 {
			tokens = append(tokens, token)
		}
	}

	return tokens
}
