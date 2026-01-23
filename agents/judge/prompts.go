/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// goldenPrompt is the prompt for golden mode judgment
var goldenPrompt = promptbuilder.MustNewPrompt(`<task>
You are evaluating a response against a reference answer.
Score the response based on the specific criterion provided.
</task>

{{golden_answer}}

{{actual_response}}

{{criterion}}

<instructions>
1. Compare the actual response to the golden answer
2. Evaluate specifically for the given criterion
3. Provide a score from 0.0 to 1.0 using this scoring rubric:

SCORING RUBRIC:
- Score 1.0 (Perfect): Response achieves the same quality and effectiveness as golden answer, or exceeds it while maintaining appropriateness.
  * Perfect Score Criteria: Semantic equivalence, equal or superior effectiveness, and functional parity or enhancement. Minor word order, synonym usage, or stylistic variations that don't affect meaning or quality should score 1.0, not be penalized. Responses that provide additional helpful information or improved clarity while meeting the criterion should also score 1.0.
  * Reasoning: Focus on how the response achieves the standard perfectly
  * Tone: Positive, confirming excellence
  * Suggestion Guidance: MUST be empty array (no improvements needed)
  * Reasoning Example: "The response provides complete, accurate information with clear instructions that perfectly match the expected format and tone."

- Score 0.75-0.99 (High Quality): Response meets criteria well with minor variations that prevent perfection.
  * Score Calibration: 0.90-0.99 for minor presentation issues (formatting, tone, structure), 0.75-0.89 for minor gaps in content or completeness
  * Reasoning: Acknowledge the response's strengths, note minor differences without treating them as flaws
  * Tone: Positive overall, mild observations about variations
  * Suggestion Guidance: MUST provide specific minor improvements that justify why the score is less than 1.0
  * Reasoning Example: "The response clearly communicates the information. While phrasing differs slightly from the reference, it maintains the same effectiveness."

- Score 0.50-0.74 (Adequate): Response partially meets criteria with notable gaps or issues.
  * Reasoning: Balance strengths and weaknesses, explain what prevents a higher score
  * Tone: Constructive, identifying specific areas for improvement
  * Suggestion Guidance: MUST provide specific improvements addressing the notable gaps identified
  * Reasoning Example: "The response addresses the main point but lacks important details about timing and specific steps, making it less comprehensive than needed."

- Score 0.25-0.49 (Poor): Response has significant problems but contains some correct elements.
  * Reasoning: Clearly identify major issues while acknowledging any correct aspects
  * Tone: Critical but constructive, focusing on substantial improvements needed
  * Suggestion Guidance: MUST provide multiple specific improvements addressing major problems
  * Reasoning Example: "While the response mentions the correct topic, it contains several factual errors and fails to address key aspects of the question."

- Score 0.0-0.24 (Failing): Response fails to meet basic criteria or contains major errors.
  * Reasoning: Explain fundamental failures, identify what needs complete correction
  * Tone: Direct about serious problems, focus on core requirements missed
  * Suggestion Guidance: MUST provide comprehensive improvements addressing fundamental failures
  * Reasoning Example: "The response is completely off-topic and provides incorrect information that directly contradicts the expected answer."

4. Explain your reasoning and provide suggestions following the guidelines above
</instructions>

<output_format>
Return your judgment as a JSON object with this structure:
{
  "mode": "golden",
  "score": 0.0-1.0,
  "reasoning": "explanation of the score for this criterion",
  "suggestions": ["improvement1", "improvement2", ...]
}

IMPORTANT: Always include "mode": "golden" in your response.

Note on suggestions:
- Focus on specific, missing elements rather than general advice
- Avoid redundant suggestions (e.g., if you've listed specific missing elements, don't add a general "be more comprehensive" suggestion)
- Each suggestion should address a distinct aspect of improvement
</output_format>

Respond with only the JSON object, no additional text.`)

// benchmarkPrompt is the prompt for benchmark mode judgment
var benchmarkPrompt = promptbuilder.MustNewPrompt(`<task>
You are evaluating two responses to determine which one better meets the evaluation criterion.
Compare the responses directly and provide a comparative assessment.
</task>

{{foo}}

{{bar}}

{{criterion}}

<instructions>
1. Evaluate responses SOLELY based on the given criterion - ignore all other response qualities
2. Compare how well each response meets the specific criterion requirements
3. A response that completely fails the criterion warrants extreme scores, regardless of other merits
4. Provide a score from -1.0 to 1.0 using this comparative scoring rubric:

IMPORTANT: Score ONLY how well each response meets the stated criterion.
Do not consider overall response quality, factual accuracy, or other aspects unless they directly relate to the criterion.

SCORING RUBRIC:
- Score -1.0 (Absolute Foo Victory): Foo response completely dominates bar response.
  * Dominance Criteria: Foo achieves the criterion perfectly while bar fundamentally fails or has major errors
  * Reasoning: Focus on how foo excels and why bar fails the basic requirements
  * Tone: Direct about bar's fundamental problems while highlighting foo's excellence
  * Suggestion Guidance: MUST provide comprehensive improvements for bar to meet minimum standards
  * Reasoning Example: "Foo provides complete, accurate analysis while bar contains factual errors and completely misses the main requirement."

- Score -0.60 to -0.99 (Foo Much Better): Foo response significantly outperforms bar response.
  * Superiority Criteria: Clear advantages in quality, completeness, or effectiveness
  * Reasoning: Focus on specific ways foo excels over bar
  * Tone: Positive about foo's strengths, constructive about bar's limitations
  * Suggestion Guidance: MUST provide specific improvements for bar to match foo quality
  * Reasoning Example: "Foo perfectly meets the [criterion] while bar completely fails to address the [criterion] requirements."

- Score -0.20 to -0.59 (Foo Somewhat Better): Foo response performs notably better with meaningful differences.
  * Advantage Criteria: Noticeable benefits in meeting the criterion, some aspects where foo clearly superior
  * Reasoning: Explain specific advantages of foo while acknowledging bar's strengths
  * Tone: Balanced, recognizing both responses' merits while noting foo's edge
  * Suggestion Guidance: MUST provide key improvements to bring bar closer to foo level
  * Reasoning Example: "Both responses meet the [criterion], but foo provides clearer structure and more actionable details."

- Score -0.19 to -0.01 (Foo Very Slightly Better): Foo has very minor advantages.
  * Minor Advantage Criteria: Subtle improvements where foo is marginally superior, but responses are largely equivalent
  * Reasoning: Acknowledge near-equivalence while noting minor advantages of foo
  * Tone: Balanced, recognizing both responses' merits while noting subtle foo edge
  * Suggestion Guidance: Optional minor improvements for bar to match foo level
  * Reasoning Example: "Both responses meet the [criterion], but foo has slightly clearer phrasing that marginally improves understanding."

- Score 0.0 (Perfectly Equivalent): Both responses are truly equivalent in quality and effectiveness.
  * Equivalence Criteria: Identical or semantically equivalent responses with no meaningful quality difference
  * Reasoning: Acknowledge that both responses achieve the same effectiveness
  * Tone: Neutral, recognizing equal merit of both approaches
  * Suggestion Guidance: No suggestions required for equivalent responses
  * Reasoning Example: "Both responses meet the [criterion] equally well with no meaningful difference in how they address the requirements."

- Score 0.01 to 0.19 (Bar Very Slightly Better): Bar has very minor advantages.
  * Minor Advantage Criteria: Subtle improvements where bar is marginally superior, but responses are largely equivalent
  * Reasoning: Acknowledge near-equivalence while noting minor advantages of bar
  * Tone: Balanced, recognizing both responses' merits while noting subtle bar edge
  * Suggestion Guidance: Optional minor improvements for foo to match bar level
  * Reasoning Example: "Both responses meet the [criterion], but bar has slightly clearer phrasing that marginally improves understanding."

- Score 0.20 to 0.59 (Bar Somewhat Better): Bar response performs notably better with meaningful advantages.
  * Advantage Criteria: Noticeable improvements over foo, some aspects where bar clearly superior
  * Reasoning: Explain specific advantages of bar while acknowledging foo's strengths
  * Tone: Balanced, recognizing both responses' merits while noting bar's edge
  * Suggestion Guidance: MUST provide key improvements for foo to match bar level
  * Reasoning Example: "Both responses meet the [criterion], but bar provides clearer structure and more actionable details."

- Score 0.60 to 0.99 (Bar Much Better): Bar response significantly outperforms foo response.
  * Superiority Criteria: Clear advantages in quality, completeness, or effectiveness
  * Reasoning: Focus on specific ways bar excels over foo
  * Tone: Positive about bar's strengths, constructive about foo's limitations
  * Suggestion Guidance: MUST provide specific improvements for foo to match bar quality
  * Reasoning Example: "Bar perfectly meets the [criterion] while foo completely fails to address the [criterion] requirements."

- Score 1.0 (Absolute Bar Victory): Bar response completely dominates foo response.
  * Dominance Criteria: Bar achieves the criterion perfectly while foo fundamentally fails or has major errors
  * Reasoning: Focus on how bar excels and why foo fails the basic requirements
  * Tone: Direct about foo's fundamental problems while highlighting bar's excellence
  * Suggestion Guidance: MUST provide comprehensive improvements for foo to meet minimum standards
  * Reasoning Example: "Bar provides complete, accurate analysis while foo contains factual errors and completely misses the main requirement."

4. Explain your reasoning and provide suggestions following the guidelines above
</instructions>

<output_format>
Return your judgment as a JSON object with this structure:
{
  "mode": "benchmark",
  "score": -1.0 to 1.0,
  "reasoning": "explanation of the comparative assessment for this criterion",
  "suggestions": ["improvement1", "improvement2", ...]
}

IMPORTANT: Always include "mode": "benchmark" in your response.

Focus suggestions on the weaker-performing response, or provide balanced suggestions for both if they're equivalent.
</output_format>

Respond with only the JSON object, no additional text.`)

// standalonePrompt is the prompt for standalone mode judgment
var standalonePrompt = promptbuilder.MustNewPrompt(`<task>
You are evaluating a response to determine how well it meets the evaluation criterion.
Assess the response's quality based on the specific criterion provided.
</task>

{{response}}

{{criterion}}

<instructions>
1. Evaluate the response SOLELY based on the given criterion - ignore all other response qualities
2. Assess how well the response meets the specific criterion requirements
3. Provide a score from 0.0 to 1.0 using this scoring rubric:

IMPORTANT: Score ONLY how well the response meets the stated criterion.
Do not consider other aspects unless they directly relate to the criterion.

SCORING RUBRIC:
- Score 1.0 (Perfect): Response perfectly meets the criterion.
  * Use When: Response fully satisfies all criterion requirements with no meaningful gaps
  * Reasoning: Focus on how the response excellently addresses the criterion
  * Tone: Positive, confirming criterion fulfillment
  * Suggestion Guidance: MUST be empty array (no improvements needed)
  * Reasoning Example: "Response perfectly meets the [criterion] with comprehensive coverage and clear execution."

- Score 0.75-0.99 (High Quality): Response meets criterion well with minor variations.
  * Use When: Response addresses criterion effectively but has small gaps or minor presentation issues
  * Reasoning: Acknowledge criterion compliance while noting minor areas for enhancement
  * Tone: Positive overall, mild observations about variations
  * Suggestion Guidance: MUST provide specific minor improvements that justify the deduction
  * Reasoning Example: "Response effectively meets the [criterion] but could be enhanced with minor improvements."

- Score 0.50-0.74 (Adequate): Response partially meets criterion with notable gaps.
  * Use When: Response addresses basic criterion requirements but missing important elements
  * Reasoning: Balance criterion compliance and gaps, explain what prevents higher score
  * Tone: Constructive, identifying specific areas needing improvement
  * Suggestion Guidance: MUST provide specific improvements addressing notable gaps
  * Reasoning Example: "Response partially meets the [criterion] but lacks important elements for full compliance."

- Score 0.25-0.49 (Poor): Response has significant problems meeting the criterion.
  * Use When: Response shows some understanding of criterion but fails in major ways
  * Reasoning: Clearly identify major criterion failures while acknowledging any correct aspects
  * Tone: Critical but constructive, focusing on substantial improvements needed
  * Suggestion Guidance: MUST provide multiple specific improvements addressing major problems
  * Reasoning Example: "Response shows some awareness of [criterion] but fails to meet requirements in significant ways."

- Score 0.0-0.24 (Failing): Response fails to meet criterion or contradicts it.
  * Use When: Response completely ignores criterion requirements or actively contradicts them
  * Reasoning: Explain fundamental criterion failures and what needs complete correction
  * Tone: Direct about serious problems, focus on core requirements missed
  * Suggestion Guidance: MUST provide comprehensive improvements addressing fundamental failures
  * Reasoning Example: "Response completely fails to meet the [criterion] and requires fundamental restructuring."

4. Explain your reasoning and provide suggestions following the guidelines above
</instructions>

<output_format>
Return your judgment as a JSON object with this structure:
{
  "mode": "standalone",
  "score": 0.0 to 1.0,
  "reasoning": "explanation of how well the response meets the criterion",
  "suggestions": ["improvement1", "improvement2", ...]
}

IMPORTANT: Always include "mode": "standalone" in your response.

Focus suggestions on how to better meet the criterion requirements.
</output_format>

Respond with only the JSON object, no additional text.`)

// Bind implements promptbuilder.Bindable for Request
func (r *Request) Bind(prompt *promptbuilder.Prompt) (*promptbuilder.Prompt, error) {
	var err error

	switch r.Mode {
	case GoldenMode:
		if prompt, err = prompt.BindXML("golden_answer", struct {
			XMLName struct{} `xml:"golden_answer"`
			Content string   `xml:",chardata"`
		}{
			Content: r.ReferenceAnswer,
		}); err != nil {
			return nil, err
		}

		if prompt, err = prompt.BindXML("actual_response", struct {
			XMLName struct{} `xml:"actual_response"`
			Content string   `xml:",chardata"`
		}{
			Content: r.ActualAnswer,
		}); err != nil {
			return nil, err
		}

	case BenchmarkMode:
		if prompt, err = prompt.BindXML("foo", struct {
			XMLName struct{} `xml:"foo"`
			Content string   `xml:",chardata"`
		}{
			Content: r.ReferenceAnswer,
		}); err != nil {
			return nil, err
		}

		if prompt, err = prompt.BindXML("bar", struct {
			XMLName struct{} `xml:"bar"`
			Content string   `xml:",chardata"`
		}{
			Content: r.ActualAnswer,
		}); err != nil {
			return nil, err
		}

	case StandaloneMode:
		if prompt, err = prompt.BindXML("response", struct {
			XMLName struct{} `xml:"response"`
			Content string   `xml:",chardata"`
		}{
			Content: r.ActualAnswer,
		}); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unknown judgment mode: %s", r.Mode)
	}

	// Bind criterion for all modes
	return prompt.BindXML("criterion", struct {
		XMLName struct{} `xml:"criterion"`
		Content string   `xml:",chardata"`
	}{
		Content: r.Criterion,
	})
}
