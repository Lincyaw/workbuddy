"""Workbuddy result submission tool.

Registers a ``submit_result`` tool the agent calls when its task is
done. The tool writes a ``RESULT: {...}`` line to stdout that the
workbuddy agentm backend parses to drive the state machine (label
transition, artifact collection, session log capture).

This replaces ``gh issue edit --add-label / --remove-label`` in agent
prompts: the agent declares WHAT state to transition to, workbuddy's
coordinator handles the actual label write. No GitHub credentials
needed in the agent sandbox for label manipulation.

Deployment: the atom file is COPY'd into ``$AGENTM_HOME/contrib/extensions/``
in the Docker image and listed in each agent's ``extensions:`` block in the
Helm chart configmap.
"""

from __future__ import annotations

import json
import sys
from typing import Any, Final

from agentm.core.abi import FunctionTool, TextContent, ToolResult, ToolTerminate
from agentm.core.abi.extension import ExtensionAPI
from agentm.extensions import ExtensionManifest

MANIFEST = ExtensionManifest(
    name="submit_result",
    description=(
        "Workbuddy result submission tool. The agent calls this when "
        "its task is complete (or failed) to signal the next state "
        "transition. Replaces manual gh issue edit label commands."
    ),
    registers=("tool:submit_result",),
    config_schema={
        "type": "object",
        "properties": {},
        "additionalProperties": False,
    },
    requires=(),
)

_TOOL_SCHEMA: Final[dict[str, Any]] = {
    "type": "object",
    "required": ["success", "next_label"],
    "additionalProperties": False,
    "properties": {
        "success": {
            "type": "boolean",
            "description": (
                "Whether you completed the task successfully. Set false "
                "if you could not fulfill the requirements."
            ),
        },
        "next_label": {
            "type": "string",
            "description": (
                "The status label to transition to. Must be a valid "
                "status:* value from the workflow (e.g. 'status:reviewing', "
                "'status:developing', 'status:blocked'). The orchestrator "
                "handles the actual label change — do NOT run gh issue edit "
                "for labels yourself."
            ),
        },
        "failure_reason": {
            "type": "string",
            "description": (
                "Short explanation of why the task failed. Required when "
                "success is false."
            ),
        },
        "summary": {
            "type": "string",
            "description": (
                "One-line summary of what you did. Will be posted as a "
                "comment on the issue by the orchestrator."
            ),
        },
    },
}


def install(api: ExtensionAPI, config: dict[str, Any]) -> None:
    del config

    async def _execute(args: dict[str, Any]) -> ToolTerminate | ToolResult:
        success = args.get("success", True)
        next_label = args.get("next_label", "")
        failure_reason = args.get("failure_reason", "")
        summary = args.get("summary", "")

        if not next_label:
            return ToolResult(
                content=[TextContent(type="text", text="next_label is required")],
                is_error=True,
            )

        if not next_label.startswith("status:"):
            return ToolResult(
                content=[TextContent(
                    type="text",
                    text=f"next_label must start with 'status:', got '{next_label}'",
                )],
                is_error=True,
            )

        if not success and not failure_reason:
            return ToolResult(
                content=[TextContent(
                    type="text",
                    text="failure_reason is required when success is false",
                )],
                is_error=True,
            )

        result_obj: dict[str, Any] = {
            "success": success,
            "next_label": next_label,
        }
        if failure_reason:
            result_obj["failure_reason"] = failure_reason

        result_line = f"RESULT: {json.dumps(result_obj)}"
        print(result_line, file=sys.stdout, flush=True)

        status = "completed successfully" if success else f"failed: {failure_reason}"
        msg = f"Result submitted. Status: {status}. Transition: {next_label}."
        if summary:
            msg += f" Summary: {summary}"

        return ToolTerminate(
            result=ToolResult(
                content=[TextContent(type="text", text=msg)],
                is_error=False,
            ),
            reason="workbuddy:result-submitted",
        )

    api.register_tool(
        FunctionTool(
            name="submit_result",
            description=(
                "Submit your task result to the workbuddy orchestrator. Call "
                "this ONCE when you are done with your task. This triggers the "
                "state transition — do NOT use gh issue edit to change labels. "
                "The orchestrator handles label changes based on your next_label."
            ),
            parameters=_TOOL_SCHEMA,
            fn=_execute,
        )
    )


__all__: Final = ["MANIFEST", "install"]
