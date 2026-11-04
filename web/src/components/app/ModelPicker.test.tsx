import { describe, expect, it } from "vitest";
import { modelParts } from "./ModelPicker";

describe("modelParts", () => {
  it.each([
    ["cc/claude-opus-5-5", "Anthropic", "Claude Opus 5.5", "cc"],
    ["claude-haiku-5-5", "Anthropic", "Claude Haiku 5.5", ""],
    ["ds/deepseek-v4-1-flash", "DeepSeek", "DeepSeek V4.1 Flash", "ds"],
    ["gc/gemini-3-8-flash", "Google", "Gemini 3.8 Flash", "gc"],
    ["deepseek-v4.1-flash", "DeepSeek", "DeepSeek V4.1 Flash", ""],
    ["claude-sonnet-5-5", "Anthropic", "Claude Sonnet 5.5", ""],
    ["gemini-3.8-flash", "Google", "Gemini 3.8 Flash", ""],
    ["gpt-5", "OpenAI", "GPT 5", ""],
  ])("%s", (id, vendor, name, route) => {
    const p = modelParts(id);
    expect([p.vendor.label, p.name, p.route]).toEqual([vendor, name, route]);
  });
});
