//! The SDK against the real API. Off unless `COMPUTERUSE_LIVE=1`; see
//! `sdk/scripts/live.sh`, which reads the token and runs this.
//!
//! It creates one session and deletes it, whatever happens in between.

use computeruse::{
    Client, ComputerUseError, Management, ManagementMode, PolicyInput, PolicyState, Session,
    SessionState,
};
use std::sync::Arc;
use std::time::{Duration, Instant};

/// Runs `echo` on the desktop through the `exec` server and prints its
/// status and output.
const EXEC_JS: &str = r#"
const call = async (tool, args) => JSON.parse((await mcp.callTool("exec", tool, args)).content[0].text);
const { id } = await call("exec", { bin: "echo", args: ["hello-from-exec"], timeout: 30 });
let logs = "", offset = 0, status = "running";
for (let i = 0; i < 200 && (status === "running" || status === "started"); i++) {
  const r = await call("stream_logs", { id, offset });
  logs += r.logs; offset = r.next_offset; status = r.status;
}
console.log(status, logs);
"#;

const BROWSER_JS: &str = r#"
const r = await mcp.callTool("browser", "browser_execute", { operations: [
  { type: "navigate", params: { url: "https://example.com" } },
  { type: "evaluate", params: { script: "document.title" } },
] });
console.log(r.content[0].text);
"#;

fn step(name: &str) {
    println!("live: {name}");
}

/// A token writes a policy only as its manager.
fn managed(source: String) -> PolicyInput {
    PolicyInput {
        source,
        management: Some(Management {
            mode: ManagementMode::Iac,
            managed_url: Some("https://github.com/r33drichards/computer-use".into()),
        }),
        if_match_version: None,
    }
}

async fn put_and_wait(session: &Session, source: String) -> Result<(), ComputerUseError> {
    let saved = session.put_policy(managed(source)).await?;
    let started = Instant::now();
    let mut state = saved.state;
    while state != PolicyState::Ready {
        assert!(
            started.elapsed() < Duration::from_secs(90),
            "the policy is {state:?} after 90s"
        );
        tokio::time::sleep(Duration::from_secs(1)).await;
        state = session.policy().await?.state;
    }
    Ok(())
}

async fn scenario(client: &Client, session: &Arc<Session>) -> Result<(), ComputerUseError> {
    let created = session.last_info().expect("create returns the session");
    assert!(!created.name.is_empty(), "the service names a session");
    println!(
        "live: session {} ({}) is {:?}",
        created.id, created.name, created.state
    );

    step("wait until running");
    let info = session.wait_until_running(Some(300_000)).await?;
    assert_eq!(info.state, SessionState::Running);

    step("run_js: console output");
    let result = session
        .run_js("console.log('answer', 6 * 7)".into())
        .await?;
    assert_eq!(result.error, None, "{}", result.raw_json);
    assert!(result.output.contains("answer 42"), "{}", result.raw_json);

    step("run_js: browser_execute");
    let result = session.run_js(BROWSER_JS.into()).await?;
    assert_eq!(result.error, None, "{}", result.raw_json);
    assert!(
        result.output.contains("Example Domain"),
        "{}",
        result.raw_json
    );

    step("run_js: exec, then stream_logs");
    let result = session.run_js(EXEC_JS.into()).await?;
    assert_eq!(result.error, None, "{}", result.raw_json);
    assert!(
        result.output.contains("hello-from-exec"),
        "{}",
        result.raw_json
    );

    step("list_tools and call_tool");
    let tools = session.list_tools().await?;
    assert!(tools.iter().any(|t| t.name == "run_js"), "{tools:?}");
    let listed = session.call_tool("list_artifacts".into(), None).await?;
    assert!(!listed.content.is_empty());

    step("policy: get");
    let policy = session.policy().await?;
    println!(
        "live: policy is {:?}, version {:?}",
        policy.state, policy.version
    );

    step("policy: browser-only, and exec is denied");
    let presets = client.policy_presets().await?;
    let preset = |id: &str| {
        presets
            .iter()
            .find(|p| p.id == id)
            .unwrap_or_else(|| panic!("no preset {id}"))
            .source
            .clone()
    };
    put_and_wait(session, preset("browser-only")).await?;
    match session.run_js(EXEC_JS.into()).await {
        Ok(result) => {
            println!("live: denied exec answered: {}", result.raw_json);
            assert!(
                !result.output.contains("hello-from-exec"),
                "exec ran under browser-only"
            );
            assert!(
                result.error.is_some(),
                "a denied call is an error of the program"
            );
        }
        Err(error) => println!("live: denied exec failed the call: {error}"),
    }

    step("policy: unrestricted again, and exec works");
    put_and_wait(session, preset("unrestricted")).await?;
    let result = session.run_js(EXEC_JS.into()).await?;
    assert!(
        result.output.contains("hello-from-exec"),
        "{}",
        result.raw_json
    );

    step("sleep");
    let asleep = session.sleep().await?;
    println!(
        "live: after sleep: {:?}, state_saved {:?}",
        asleep.state, asleep.state_saved
    );
    assert!(
        matches!(asleep.state, SessionState::Stopping | SessionState::Asleep),
        "{asleep:?}"
    );
    assert_eq!(asleep.state_saved, Some(true), "the sleep took a snapshot");

    step("wake");
    let waking = session.wake().await?;
    assert!(
        matches!(waking.state, SessionState::Starting | SessionState::Running),
        "{waking:?}"
    );
    session.wait_until_running(Some(300_000)).await?;

    step("rename");
    let renamed = session.rename("sdk-live-renamed".into()).await?;
    assert_eq!(renamed.name, "sdk-live-renamed");
    Ok(())
}

#[tokio::test]
async fn live() {
    if std::env::var("COMPUTERUSE_LIVE").as_deref() != Ok("1") {
        eprintln!("skipped: set COMPUTERUSE_LIVE=1 (see sdk/scripts/live.sh)");
        return;
    }
    let token = std::env::var("COMPUTERUSE_API_TOKEN").expect("COMPUTERUSE_API_TOKEN");
    let mut builder = Client::builder().api_token(&token);
    if let Ok(base_url) = std::env::var("COMPUTERUSE_BASE_URL") {
        builder = builder.base_url(base_url);
    }
    let client = builder.build().unwrap();

    step("token exchange");
    let access = client.access_token(false).await.unwrap();
    assert!(
        !access.is_empty() && access != token,
        "an access token, not the API token"
    );

    step("me");
    let me = client.me().await.unwrap();
    assert!(!me.email.is_empty());
    assert!(!me.admin, "a token is never an admin");

    step("create");
    let session = client.sessions().create().send().await.unwrap();

    // In a task, so that a failed assertion does not skip the delete.
    let outcome = tokio::spawn({
        let (client, session) = (client.clone(), session.clone());
        async move { scenario(&client, &session).await }
    })
    .await;

    // Whatever happened: what was created is deleted.
    step("delete");
    session.delete().await.unwrap();
    match session.refresh().await {
        Err(ComputerUseError::NotFound { .. }) => {}
        other => println!("live: after delete, a read answered {other:?}"),
    }
    outcome.expect("the scenario panicked").unwrap();
}
