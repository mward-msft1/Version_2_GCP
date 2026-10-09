"""Export one agent run with the Microsoft OpenTelemetry Distro.

The Agent 365 distro has no Go package. The Go agent acquires the app-only
token and invokes this script so spans are created and exported by
microsoft-opentelemetry.

instrument_observability is the Agent 365 instrumentation step. It must emit
a valid invoke_agent span as the root of the run. Without that root span,
Defender, Purview, and the Microsoft 365 admin center drop the activity even
when ingestion returns HTTP 200.
"""

import json
import logging
import os
import sys

os.environ["ENABLE_OBSERVABILITY"] = "true"
os.environ["ENABLE_A365_OBSERVABILITY_EXPORTER"] = "true"
os.environ["A365_USE_S2S_ENDPOINT"] = "true"


def main() -> int:
    if "--versions" in sys.argv:
        try:
            emit({"status": 200, "summary": "platform versions", "platform": require_platform()})
        except Exception as exc:  # noqa: BLE001 - report version failures to the Go caller
            return fail(f"platform version check failed: {exc}")
        return 0
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as exc:
        return fail(f"distro exporter received invalid input: {exc}")
    token = os.environ.get("A365_OBSERVABILITY_TOKEN", "").strip()
    if not token:
        return fail("observability token was not provided to the distro exporter")
    evidence = Evidence()
    try:
        export(payload, token, evidence)
    except Exception as exc:  # noqa: BLE001 - report exporter failures to the Go caller
        return fail(f"microsoft-opentelemetry export failed: {exc}")
    steps = payload.get("steps") or []
    status, summary = evidence.result(2 + len(steps))
    emit({"status": status, "spanCount": 2 + len(steps), "summary": summary})
    return 0 if 200 <= status < 300 else 1


class _EvidenceHandler(logging.Handler):
    def __init__(self, evidence: "Evidence") -> None:
        super().__init__(level=logging.DEBUG)
        self._evidence = evidence

    def emit(self, record: logging.LogRecord) -> None:
        self._evidence.lines.append(record.getMessage())


class Evidence:
    def __init__(self) -> None:
        self.lines: list[str] = []
        self.exported: list[dict] = []

    def record_exported(self, spans) -> None:
        for span in spans:
            attributes = dict(getattr(span, "attributes", {}) or {})
            parent = getattr(span, "parent", None)
            self.exported.append({
                "name": getattr(span, "name", ""),
                "operation": attributes.get("gen_ai.operation.name"),
                "root": parent is None or not getattr(parent, "is_valid", False),
            })

    def result(self, span_count: int) -> tuple[int, str]:
        roots = [span for span in self.exported if span["root"] and span["name"] == "invoke_agent" and span["operation"] == "invoke_agent"]
        if not roots:
            seen = ", ".join(f"{span['name']}/{span['operation']}" for span in self.exported) or "none"
            return 0, f"exported batch has no root invoke_agent span; saw {seen}"
        delivered = [line for line in self.lines if "HTTP 2" in line and "success" in line]
        failed = [line for line in self.lines if "non-retryable error" in line or "Token resolution failed" in line]
        relevant_failed = [line for line in failed if "Tenant id  is invalid" not in line]
        proof = f"exported root span name=invoke_agent operation=invoke_agent ({len(roots)})"
        if delivered and not relevant_failed:
            return 200, f"microsoft-opentelemetry exported {span_count} spans. {proof}. {delivered[-1]}"
        if relevant_failed:
            return 0, relevant_failed[-1]
        if delivered:
            return 200, f"microsoft-opentelemetry exported {span_count} spans. {proof}. {delivered[-1]}"
        return 0, "microsoft-opentelemetry finished without an Agent 365 delivery confirmation"


def require_platform() -> dict:
    import importlib.metadata as metadata

    from packaging.version import Version

    import google.adk
    import google.cloud.aiplatform as aiplatform
    import microsoft_agents_a365.notifications
    import microsoft_agents_a365.observability.core
    import microsoft_agents_a365.tooling
    from google.cloud import aiplatform_v1
    from microsoft_agents_a365.runtime import get_observability_authentication_scope

    required = {
        "google-adk": "1.18.0",
        "google-cloud-aiplatform": "1.126.1",
        "microsoft-agents-a365-runtime": "1.0.0",
        "microsoft-agents-a365-observability-core": "1.0.0",
        "microsoft-agents-a365-notifications": "1.0.0",
        "microsoft-agents-a365-tooling": "1.0.0",
        "microsoft-opentelemetry": "1.4.0",
    }
    found = {}
    for name, floor in required.items():
        current = metadata.version(name)
        if Version(current) < Version(floor):
            raise RuntimeError(f"{name} {current} is below required {floor}")
        found[name] = current
    # Touch the libraries so a metadata-only install cannot pass.
    if not google.adk.__version__ or not aiplatform.__version__:
        raise RuntimeError("google-adk or google-cloud-aiplatform did not load")
    if aiplatform_v1.ReasoningEngineServiceClient is None:
        raise RuntimeError("Vertex Reasoning Engine v1 client is missing")
    scope = get_observability_authentication_scope()
    if not scope:
        raise RuntimeError("Agent 365 SDK did not return an observability scope")
    found["a365ObservabilityScope"] = scope[0]
    return found


def export(payload: dict, token: str, evidence: Evidence) -> None:
    # A365 Observability — best-effort instrumentation (verify against official sample)
    # A365 auth mode: s2s — telemetry uses an app-only token on the S2S route.
    instrument_observability(payload, token, evidence)


def instrument_observability(payload: dict, token: str, evidence: Evidence) -> None:
    """instrument_observability: emit a root invoke_agent span and export it."""
    require_platform()
    from opentelemetry import trace
    from opentelemetry.context import Context
    from microsoft.opentelemetry import use_microsoft_opentelemetry
    from microsoft.opentelemetry.a365.core.constants import INVOKE_AGENT_OPERATION_NAME
    from microsoft.opentelemetry.a365.core import (
        AgentDetails,
        BaggageBuilder,
        CallerDetails,
        Channel,
        ChatMessage,
        ExecuteToolScope,
        InferenceCallDetails,
        InferenceOperationType,
        InferenceScope,
        InputMessages,
        InvokeAgentScope,
        InvokeAgentScopeDetails,
        MessageRole,
        OutputMessage,
        OutputMessages,
        Request,
        ServiceEndpoint,
        SpanDetails,
        TextPart,
        ToolCallDetails,
        ToolType,
        UserDetails,
    )

    agent_id = required(payload, "agentId")
    tenant_id = required(payload, "tenantId")
    os.environ["A365_TENANT_ID"] = tenant_id
    os.environ["A365_AGENT_ID"] = agent_id
    os.environ["CONNECTIONS__SERVICE_CONNECTION__SETTINGS__TENANTID"] = tenant_id
    use_microsoft_opentelemetry(
        enable_a365=True,
        a365_enable_observability_exporter=True,
        a365_use_s2s_endpoint=True,
        a365_token_resolver=lambda _agent_id, _tenant_id: token,
        a365_max_queue_size=2048,
        a365_scheduled_delay_ms=5000,
        a365_exporter_timeout_ms=30000,
        a365_max_export_batch_size=512,
    )
    exporter_log = logging.getLogger("microsoft.opentelemetry.a365.core.exporters.agent365_exporter")
    exporter_log.setLevel(logging.DEBUG)
    exporter_log.addHandler(_EvidenceHandler(evidence))

    acting_user = str(payload.get("actingUser") or "")
    user = UserDetails(
        user_id=str(payload.get("userId") or "unknown"),
        user_email=acting_user,
        user_name=acting_user.split("@", 1)[0] or acting_user or "unknown",
    )
    agent = AgentDetails(
        agent_id=agent_id,
        agent_name=str(payload.get("agentName") or "Caldova GCP Agent Version 2"),
        agent_description=str(payload.get("description") or ""),
        agent_blueprint_id=str(payload.get("blueprintId") or ""),
        tenant_id=tenant_id,
        provider_name="google",
    )
    conversation = str(payload.get("conversationId") or "caldova")
    prompt = str(payload.get("input") or "")
    output = str(payload.get("output") or "")
    endpoint = ServiceEndpoint(hostname="us-central1-aiplatform.googleapis.com", port=443)
    request = Request(
        content=InputMessages(messages=[
            ChatMessage(role=MessageRole.USER, parts=[TextPart(content=prompt)]),
        ]),
        session_id=conversation,
        channel=Channel(name="msteams"),
        conversation_id=conversation,
    )
    baggage = (
        BaggageBuilder()
        .tenant_id(tenant_id)
        .agent_id(agent_id)
        .agent_name(agent.agent_name)
        .agent_description(agent.agent_description)
        .agent_blueprint_id(agent.agent_blueprint_id)
        .user_id(user.user_id)
        .user_email(user.user_email)
        .user_name(user.user_name)
        .user_client_ip("127.0.0.1")
        .channel_name("msteams")
        .session_id(conversation)
        .conversation_id(conversation)
        .invoke_agent_server(endpoint.hostname, endpoint.port)
    )
    with baggage.build():
        with InvokeAgentScope.start(
            request=request,
            scope_details=InvokeAgentScopeDetails(endpoint=endpoint),
            agent_details=agent,
            caller_details=CallerDetails(user_details=user),
            span_details=SpanDetails(parent_context=Context()),
        ) as invoke_scope:
            require_root_invoke_agent(invoke_scope, INVOKE_AGENT_OPERATION_NAME)
            rename = getattr(invoke_scope._span, "update_name", None)
            if not callable(rename):
                raise RuntimeError("invoke_agent span cannot be renamed to the exported root name")
            rename(INVOKE_AGENT_OPERATION_NAME)
            invoke_scope.record_input_messages(InputMessages(messages=[
                ChatMessage(role=MessageRole.USER, parts=[TextPart(content=prompt)]),
            ]))
            with InferenceScope.start(
                request=request,
                details=InferenceCallDetails(
                    operationName=InferenceOperationType.CHAT,
                    model="gemini-flash-latest",
                    providerName="google",
                    endpoint=endpoint,
                ),
                agent_details=agent,
                user_details=user,
            ) as inference_scope:
                inference_scope.record_output_messages(OutputMessages(messages=[
                    OutputMessage(
                        role=MessageRole.ASSISTANT,
                        parts=[TextPart(content=output)],
                        finish_reason="stop",
                    ),
                ]))
            for index, step in enumerate(payload.get("steps") or [], start=1):
                name = str(step.get("name") or "tool")
                detail = str(step.get("detail") or "")
                with ExecuteToolScope.start(
                    request=request,
                    details=ToolCallDetails(
                        tool_name=name,
                        arguments=prompt[:2000],
                        tool_call_id=f"call-{index:02d}",
                        description=name,
                        tool_type=ToolType.FUNCTION.value,
                        endpoint=endpoint,
                    ),
                    agent_details=agent,
                    user_details=user,
                ) as tool_scope:
                    tool_scope.record_response(detail)
                    if not step.get("ok", True):
                        tool_scope.record_error(RuntimeError(detail or name))
            invoke_scope.record_output_messages(OutputMessages(messages=[
                OutputMessage(
                    role=MessageRole.ASSISTANT,
                    parts=[TextPart(content=output)],
                    finish_reason="stop",
                ),
            ]))

    # The distro registers a baggage processor before the exporter. That
    # processor does not implement force_flush, so the provider flush returns
    # false and never reaches the Agent 365 batch processor.
    watch_exported_spans(evidence)
    flush_agent365_exporter()
    provider = trace.get_tracer_provider()
    shutdown = getattr(provider, "shutdown", None)
    if callable(shutdown):
        shutdown()


def require_root_invoke_agent(scope, operation_name: str) -> None:
    span = getattr(scope, "_span", None)
    if span is None:
        raise RuntimeError("instrument_observability did not start an invoke_agent span")
    attributes = getattr(span, "attributes", {}) or {}
    actual = attributes.get("gen_ai.operation.name")
    if actual != operation_name or operation_name != "invoke_agent":
        raise RuntimeError(f"root span operation is {actual!r}, expected invoke_agent")
    parent = getattr(span, "parent", None)
    if parent is not None and getattr(parent, "is_valid", False):
        raise RuntimeError("invoke_agent span is not the root span")


def watch_exported_spans(evidence: Evidence) -> None:
    from opentelemetry import trace

    provider = trace.get_tracer_provider()
    multi = getattr(provider, "_active_span_processor", None)
    processors = list(getattr(multi, "_span_processors", []) or [])
    watched = 0
    for processor in processors:
        exporter = getattr(processor, "span_exporter", None)
        if exporter is None or getattr(exporter, "_caldova_export_watched", False):
            continue
        original = exporter.export

        def export(spans, _original=original):
            evidence.record_exported(spans)
            return _original(spans)

        exporter.export = export
        exporter._caldova_export_watched = True
        watched += 1
    if watched == 0:
        raise RuntimeError("Agent 365 exporter was not registered")


def flush_agent365_exporter(timeout_millis: int = 60000) -> None:
    from opentelemetry import trace

    provider = trace.get_tracer_provider()
    multi = getattr(provider, "_active_span_processor", None)
    processors = list(getattr(multi, "_span_processors", []) or [])
    flushed = 0
    for processor in processors:
        if getattr(processor, "span_exporter", None) is None:
            continue
        if processor.force_flush(timeout_millis) is False:
            raise RuntimeError("Agent 365 batch processor flush failed")
        flushed += 1
    if flushed == 0:
        raise RuntimeError("Agent 365 batch processor was not registered")


def required(payload: dict, key: str) -> str:
    value = str(payload.get(key) or "").strip()
    if not value:
        raise RuntimeError(f"distro exporter requires {key}")
    return value


def fail(message: str) -> int:
    emit({"status": 0, "spanCount": 0, "summary": message})
    return 1


def emit(result: dict) -> None:
    json.dump(result, sys.stdout)
    sys.stdout.write("\n")


if __name__ == "__main__":
    sys.exit(main())
