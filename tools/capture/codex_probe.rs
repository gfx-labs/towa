// Injected into openai/codex as a child module of codex-core's
// `tools::spec_plan` by tools/capture/codex.py, then run with `cargo test`.
// It calls the upstream tool planners unchanged and writes what they return.
// It is never compiled as part of acpharness.

use std::sync::Arc;

use codex_features::Feature;
use codex_features::SleepToolMode;
use codex_protocol::openai_models::ModelInfo;
use codex_protocol::openai_models::ToolMode;
use codex_protocol::protocol::MultiAgentVersion;
use codex_tools::ToolExecutor;
use codex_tools::create_tools_json_for_responses_api;
use codex_tools::create_tools_json_for_responses_lite;
use serde_json::Value;
use serde_json::json;

use super::append_source_tools;
use super::build_core_tool_registry;
use crate::session::tests::make_session_and_context;
use crate::session::tests::mcp_config_for_test;
use crate::session::tests::update_turn_settings_for_test;
use crate::session::turn_context::TurnContext;
use crate::tasks::InterruptedTurnHistoryMarker;
use crate::tasks::interrupted_turn_history_marker;
use crate::tools::router::ToolRouter;

type ExtensionExecutor =
    Arc<dyn for<'call> ToolExecutor<codex_tools::ToolCall<'call>>>;

// Every feature that tools/spec_plan.rs reads to decide whether a core tool is
// registered. Code mode is left to the model's own tool_mode.
const TOOL_FEATURES: &[Feature] = &[
    Feature::ShellTool,
    Feature::UnifiedExec,
    Feature::ViewImage,
    Feature::TokenBudget,
    Feature::RequestPermissionsTool,
    Feature::SleepTool,
    Feature::CurrentTimeReminder,
    Feature::SendMessageToUserAsync,
    Feature::DeferredExecutor,
    Feature::ExecPermissionApprovals,
];

fn enable(turn: &mut TurnContext, feature: Feature) -> bool {
    let mut config = (*turn.config).clone();
    let enabled = config.features.enable(feature).is_ok();
    if enabled {
        turn.config = Arc::new(config);
    }
    enabled
}

async fn plan(model: &ModelInfo, profile: &str) -> Value {
    let (_session, mut turn) = make_session_and_context().await;
    let mut model = codex_models_manager::model_info::with_config_overrides(
        model.clone(),
        &turn.config.to_models_manager_config(),
    );
    let mut enabled_features = Vec::new();
    let mut multi_agent_version = turn
        .config
        .multi_agent_version_for_model(model.multi_agent_version);
    if profile != "default" {
        model.tool_mode = Some(ToolMode::Direct);
    }
    if let Some(version) = profile.strip_prefix("all_features_multi_agent_") {
        for feature in TOOL_FEATURES {
            if enable(&mut turn, *feature) {
                enabled_features.push(format!("{feature:?}"));
            }
        }
        let mut config = (*turn.config).clone();
        config.experimental_request_user_input_enabled = true;
        config.update_plan_enabled = true;
        config.sleep_tool_mode = SleepToolMode::AlwaysOn;
        turn.config = Arc::new(config);
        multi_agent_version = match version {
            "v1" => MultiAgentVersion::V1,
            "v2" => MultiAgentVersion::V2,
            other => panic!("unknown multi agent version {other}"),
        };
    }
    turn.multi_agent_version = multi_agent_version;
    update_turn_settings_for_test(&mut turn, |settings| {
        settings.model_info = Arc::new(model.clone());
    });

    let mcp = codex_mcp::McpBinding::empty(mcp_config_for_test(&turn.config));
    let mut registry = build_core_tool_registry(
        &turn,
        &model,
        &turn.initial_environments,
        &mcp,
        None,
        None,
    );
    let registered = registry
        .entries()
        .map(|entry| {
            let tool_name = entry.runtime.tool_name();
            json!({
                "namespace": tool_name.namespace,
                "name": tool_name.name,
                "exposure": format!("{:?}", entry.exposure),
                "spec": serde_json::to_value(entry.spec().as_ref()).expect("serialize spec"),
            })
        })
        .collect::<Vec<_>>();
    let hosted_specs = append_source_tools(
        &turn,
        &model,
        &mut registry,
        Vec::new(),
        Vec::<ExtensionExecutor>::new(),
        &[],
    );
    let router = ToolRouter::from_registry(
        &turn,
        &model,
        registry,
        hosted_specs,
        &Default::default(),
    );
    let visible = router.model_visible_specs();
    json!({
        "model": model.slug,
        "profile": profile,
        "tool_mode": format!("{:?}", router.tool_mode()),
        "multi_agent_version": format!("{multi_agent_version:?}"),
        "use_responses_lite": model.use_responses_lite,
        "enabled_features": enabled_features,
        "registered": registered,
        "request_tools": create_tools_json_for_responses_api(&visible).expect("serialize tools"),
        "request_tools_responses_lite":
            create_tools_json_for_responses_lite(&visible).expect("serialize lite tools"),
    })
}

#[tokio::test]
async fn acpharness_capture() {
    let out = std::env::var("ACPHARNESS_CAPTURE_OUT").expect("ACPHARNESS_CAPTURE_OUT");
    let models = codex_models_manager::bundled_models_response()
        .expect("bundled models.json")
        .models;
    let mut plans = Vec::new();
    for model in &models {
        for profile in [
            "default",
            "direct",
            "all_features_multi_agent_v1",
            "all_features_multi_agent_v2",
        ] {
            plans.push(plan(model, profile).await);
        }
    }
    let marker = |marker| {
        serde_json::to_value(interrupted_turn_history_marker(marker)).expect("serialize marker")
    };
    let report = json!({
        "plans": plans,
        "turn_aborted": {
            "contextual_user": marker(InterruptedTurnHistoryMarker::ContextualUser),
            "developer": marker(InterruptedTurnHistoryMarker::Developer),
        },
    });
    std::fs::write(
        out,
        serde_json::to_string_pretty(&report).expect("serialize report"),
    )
    .expect("write report");
}
