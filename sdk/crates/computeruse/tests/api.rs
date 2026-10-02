//! Every call of the client against an in-process fake of the API host.
//! Nothing here talks to the real service, and no token here is real.

mod support;

use computeruse::{
    BuildError, Client, ClientOptions, ClientOptionsBuilder, ComputerUseError,
    CreateSessionRequest, CreateSessionRequestBuilder, Management, ManagementMode, PolicyInput,
    PolicyInputBuilder, PolicyState, RunJsRequestBuilder, SessionState,
};
use serde_json::{json, Value};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;
use support::{client, session_json, Fake, Reply, Request, TOKEN, TOKEN_ID};

const ID: &str = "s-abcde";

fn not_found() -> Reply {
    Reply::json(404, json!({"error": "session not found"}))
}

mod auth {
    use super::*;

    #[tokio::test]
    async fn exchanges_the_api_token_once_and_sends_the_access_token() {
        let fake = Fake::api(|_| {
            Reply::json(
                200,
                json!({"email": "you@example.com", "name": "You", "admin": false}),
            )
        })
        .await;
        let client = client(&fake);

        let me = client.me().await.unwrap();
        assert_eq!(me.email, "you@example.com");
        assert!(!me.admin);
        client.me().await.unwrap();

        let requests = fake.requests();
        assert_eq!(
            fake.count("POST", "/oauth/token"),
            1,
            "one exchange for two calls"
        );
        let exchange = &requests[0];
        assert!(exchange.is("POST", "/oauth/token"));
        // HTTP Basic: the token's id and the token.
        use base64::Engine;
        let basic = base64::engine::general_purpose::STANDARD.encode(format!("{TOKEN_ID}:{TOKEN}"));
        assert_eq!(exchange.header("authorization"), format!("Basic {basic}"));
        assert_eq!(
            exchange.header("content-type"),
            "application/x-www-form-urlencoded"
        );
        assert_eq!(exchange.body, "grant_type=client_credentials");

        for call in &requests[1..] {
            assert!(call.is("GET", "/v1/me"));
            assert_eq!(call.header("authorization"), "Bearer access-1");
            assert!(call.header("user-agent").starts_with("computeruse-sdk/"));
        }
    }

    #[tokio::test]
    async fn narrows_the_access_token_to_the_scopes_asked_for() {
        let fake = Fake::api(|_| Reply::json(200, json!([]))).await;
        let client = Client::builder()
            .api_token(TOKEN)
            .base_url(&fake.url)
            .scopes(["sessions:read", "sessions:connect"])
            .user_agent("my-app/1.0")
            .build()
            .unwrap();
        client.list_sessions().await.unwrap();
        let requests = fake.requests();
        assert_eq!(
            requests[0].body,
            "grant_type=client_credentials&scope=sessions%3Aread+sessions%3Aconnect"
        );
        assert!(requests[1]
            .header("user-agent")
            .starts_with("my-app/1.0 computeruse-sdk/"));
    }

    #[tokio::test]
    async fn asks_for_a_new_access_token_after_a_401() {
        // The backend restarted: access-1 is no longer good.
        let fake = Fake::api(|request| match request.header("authorization") {
            "Bearer access-1" => Reply::json(401, json!({"error": "invalid token"})),
            _ => Reply::json(200, json!({"email": "you@example.com"})),
        })
        .await;
        let client = client(&fake);
        assert_eq!(client.me().await.unwrap().email, "you@example.com");
        assert_eq!(fake.count("POST", "/oauth/token"), 2);
        assert_eq!(fake.count("GET", "/v1/me"), 2);
        assert_eq!(
            fake.requests().last().unwrap().header("authorization"),
            "Bearer access-2"
        );
    }

    #[tokio::test]
    async fn a_401_that_stays_is_unauthorized_after_one_refresh() {
        let fake = Fake::api(|_| Reply::json(401, json!({"error": "invalid token"}))).await;
        let error = client(&fake).me().await.unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::Unauthorized {
                message: "invalid token".into()
            }
        );
        assert_eq!(error.status(), Some(401));
        assert_eq!(fake.count("POST", "/oauth/token"), 2);
        assert_eq!(fake.count("GET", "/v1/me"), 2);
    }

    #[tokio::test]
    async fn replaces_an_access_token_that_has_expired() {
        let exchanges = AtomicUsize::new(0);
        let fake = Fake::start(move |request, _| {
            if request.is("POST", "/oauth/token") {
                let n = exchanges.fetch_add(1, Ordering::SeqCst) + 1;
                // No time left: the next call must ask again.
                return Reply::json(200, json!({"access_token": format!("short-{n}"), "token_type": "Bearer", "expires_in": 0, "scope": ""}));
            }
            Reply::json(200, json!({"email": request.header("authorization")}))
        })
        .await;
        let client = client(&fake);
        assert_eq!(client.me().await.unwrap().email, "Bearer short-1");
        assert_eq!(client.me().await.unwrap().email, "Bearer short-2");
        assert_eq!(client.access_token(false).await.unwrap(), "short-3");
    }

    #[tokio::test]
    async fn a_refused_exchange_is_unauthorized_and_nothing_else_is_sent() {
        let fake = Fake::start(|_, _| Reply::json(401, json!({"error": "invalid_client"}))).await;
        let error = client(&fake).me().await.unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::Unauthorized {
                message: "invalid_client".into()
            }
        );
        assert!(fake.calls().is_empty());
        assert_eq!(fake.count("POST", "/oauth/token"), 1);
    }

    #[tokio::test]
    async fn a_rate_limited_exchange_is_tried_again() {
        let fake = Fake::start(|request, n| {
            if request.is("POST", "/oauth/token") {
                return if n == 0 {
                    Reply::json(429, json!({"error": "too many attempts"})).header("retry-after", "0")
                } else {
                    Reply::json(200, json!({"access_token": "a", "token_type": "Bearer", "expires_in": 3600, "scope": ""}))
                };
            }
            Reply::json(200, json!({"email": "you@example.com"}))
        })
        .await;
        client(&fake).me().await.unwrap();
        assert_eq!(fake.count("POST", "/oauth/token"), 2);
    }

    #[tokio::test]
    async fn sends_the_api_token_itself_when_exchange_is_off() {
        let fake = Fake::api(|_| Reply::json(200, json!({"email": "you@example.com"}))).await;
        let client = Client::builder()
            .api_token(TOKEN)
            .base_url(&fake.url)
            .exchange_token(false)
            .build()
            .unwrap();
        client.me().await.unwrap();
        assert_eq!(fake.count("POST", "/oauth/token"), 0);
        assert_eq!(
            fake.requests()[0].header("authorization"),
            format!("Bearer {TOKEN}")
        );
        assert_eq!(client.access_token(true).await.unwrap(), TOKEN);
    }

    #[tokio::test]
    async fn sends_an_access_token_as_it_is() {
        let fake = Fake::api(|_| Reply::json(200, json!({"email": "you@example.com"}))).await;
        let client = Client::builder()
            .api_token("eyJhbGciOiJIUzI1NiJ9.e30.sig")
            .base_url(&fake.url)
            .build()
            .unwrap();
        client.me().await.unwrap();
        assert_eq!(fake.count("POST", "/oauth/token"), 0);
        assert_eq!(
            fake.requests()[0].header("authorization"),
            "Bearer eyJhbGciOiJIUzI1NiJ9.e30.sig"
        );
    }

    #[test]
    fn refuses_what_it_cannot_use() {
        let configuration = |result: Result<Arc<Client>, ComputerUseError>| {
            assert!(
                matches!(result, Err(ComputerUseError::Configuration { .. })),
                "{result:?}"
            );
        };
        configuration(Client::builder().build());
        configuration(Client::builder().api_token("  ").build());
        configuration(Client::builder().api_token("a b").build());
        configuration(
            Client::builder()
                .api_token(TOKEN)
                .base_url("http://api.example.test")
                .build(),
        );
        configuration(
            Client::builder()
                .api_token(TOKEN)
                .base_url("https://api.example.test/v1")
                .build(),
        );
        configuration(
            Client::builder()
                .api_token(TOKEN)
                .timeout(Duration::ZERO)
                .build(),
        );
        // Scopes narrow an exchange; without one they would be ignored.
        configuration(
            Client::builder()
                .api_token(TOKEN)
                .exchange_token(false)
                .scopes(["sessions:read"])
                .build(),
        );
        assert_eq!(
            Client::with_token(TOKEN.into()).unwrap().base_url(),
            "https://api.computeruse.site"
        );
    }

    #[test]
    fn debug_output_has_no_token() {
        let options = ClientOptionsBuilder::new()
            .api_token(TOKEN.into())
            .build()
            .unwrap();
        let builder = ClientOptionsBuilder::new().api_token(TOKEN.into());
        let fluent = Client::builder().api_token(TOKEN);
        let client = Client::new(options.clone()).unwrap();
        let session = client.session(ID.into()).unwrap();
        for printed in [
            format!("{options:?}"),
            format!("{builder:?}"),
            format!("{fluent:?}"),
            format!("{client:?}"),
            format!("{session:?}"),
        ] {
            assert!(!printed.contains("bjs_"), "{printed}");
            assert!(!printed.contains("c2VjcmV0"), "{printed}");
        }
        assert!(format!("{options:?}").contains("<redacted>"));
    }
}

mod sessions {
    use super::*;

    #[tokio::test]
    async fn lists() {
        let fake = Fake::api(|_| {
            Reply::json(
                200,
                json!([
                    session_json(ID, "brave-otter", "running"),
                    session_json("s-abcdefghij", "b", "asleep")
                ]),
            )
        })
        .await;
        let list = client(&fake).sessions().list().await.unwrap();
        assert_eq!(fake.calls(), ["GET /v1/sessions"]);
        assert_eq!(list.len(), 2);
        assert_eq!(list[0].id, ID);
        assert_eq!(list[0].name, "brave-otter");
        assert_eq!(list[0].state, SessionState::Running);
        assert_eq!(list[0].created.as_deref(), Some("2026-10-01T12:00:00Z"));
        assert_eq!(list[0].policy.as_ref().unwrap().state, PolicyState::Ready);
        assert_eq!(list[1].state, SessionState::Asleep);
    }

    #[tokio::test]
    async fn an_empty_list_written_as_null_is_empty() {
        let fake = Fake::api(|_| Reply::json(200, json!(null))).await;
        assert!(client(&fake).list_sessions().await.unwrap().is_empty());
        assert!(client(&fake).policy_presets().await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn a_state_from_the_future_is_unknown_not_an_error() {
        let fake = Fake::api(|_| {
            let mut session = session_json(ID, "a", "hibernating");
            session["stoppedBy"] = json!("credit");
            session["deleteAfter"] = json!("2026-11-01T00:00:00Z");
            session["something_new"] = json!({"x": 1});
            Reply::json(200, json!([session, {"id": "s-zzzzz", "state": "running"}]))
        })
        .await;
        let list = client(&fake).list_sessions().await.unwrap();
        assert_eq!(list[0].state, SessionState::Unknown);
        assert_eq!(list[0].stopped_by.as_deref(), Some("credit"));
        assert_eq!(
            list[0].delete_after.as_deref(),
            Some("2026-11-01T00:00:00Z")
        );
        // Only id and state are needed.
        assert_eq!(list[1].name, "");
        assert!(list[1].policy.is_none());
    }

    #[tokio::test]
    async fn creates_with_a_name() {
        let fake = Fake::api(|request| {
            Reply::json(
                201,
                session_json(
                    ID,
                    request.json()["name"].as_str().unwrap_or("brave-otter"),
                    "starting",
                ),
            )
        })
        .await;
        let client = client(&fake);

        let session = client
            .sessions()
            .create()
            .name("nightly")
            .send()
            .await
            .unwrap();
        assert_eq!(session.id(), ID);
        assert_eq!(session.mcp_url(), format!("{}/{ID}/mcp", fake.url));
        let info = session.last_info().unwrap();
        assert_eq!(info.name, "nightly");
        assert_eq!(info.state, SessionState::Starting);

        // No name: the service picks one, and the body says nothing.
        let session = client
            .create_session(CreateSessionRequest::default())
            .await
            .unwrap();
        assert_eq!(session.last_info().unwrap().name, "brave-otter");

        let posts: Vec<Request> = fake
            .requests()
            .into_iter()
            .filter(|r| r.is("POST", "/v1/sessions"))
            .collect();
        assert_eq!(posts[0].json(), json!({"name": "nightly"}));
        assert_eq!(posts[0].header("content-type"), "application/json");
        assert_eq!(posts[1].json(), json!({}));
    }

    #[tokio::test]
    async fn creates_with_a_policy_preset_by_sending_its_source() {
        let fake = Fake::api(|request| {
            if request.is("GET", "/v1/policy-presets") {
                return Reply::json(200, json!([
                    {"id": "unrestricted", "title": "Unrestricted", "description": "Everything.", "kind": "rego", "source": "package p\nallow := true"},
                    {"id": "no-scripting", "title": "No scripting", "description": "", "kind": "rego", "source": "package p\nallow := false"},
                ]));
            }
            Reply::json(201, session_json(ID, "a", "starting"))
        })
        .await;
        let client = client(&fake);
        client
            .sessions()
            .create()
            .name("x")
            .policy_preset("no-scripting")
            .send()
            .await
            .unwrap();
        assert_eq!(
            fake.calls(),
            ["GET /v1/policy-presets", "POST /v1/sessions"]
        );
        assert_eq!(
            fake.requests().last().unwrap().json(),
            json!({"name": "x", "policy": {"kind": "rego", "source": "package p\nallow := false"}})
        );

        let error = client
            .sessions()
            .create()
            .policy_preset("nope")
            .send()
            .await
            .unwrap_err();
        assert!(
            matches!(error, ComputerUseError::Configuration { ref reason } if reason.contains("nope")),
            "{error:?}"
        );
        assert_eq!(fake.count("POST", "/v1/sessions"), 1, "nothing was created");
    }

    #[tokio::test]
    async fn creates_with_a_rego_policy() {
        let fake = Fake::api(|_| Reply::json(201, session_json(ID, "a", "starting"))).await;
        let client = client(&fake);
        client
            .sessions()
            .create()
            .policy(PolicyInput {
                source: "package p".into(),
                management: Some(Management {
                    mode: ManagementMode::Iac,
                    managed_url: Some("https://git.example/policy.rego".into()),
                }),
                if_match_version: None,
            })
            .send()
            .await
            .unwrap();
        assert_eq!(
            fake.requests().last().unwrap().json(),
            json!({"policy": {"kind": "rego", "source": "package p",
                   "management": {"mode": "iac", "managed_url": "https://git.example/policy.rego"}}})
        );

        let both = client
            .sessions()
            .create()
            .policy_rego("package p")
            .policy_preset("unrestricted")
            .send()
            .await;
        assert!(matches!(both, Err(ComputerUseError::Configuration { .. })));
    }

    #[tokio::test]
    async fn gets_and_refreshes() {
        let fake = Fake::api(|request| {
            if request.path == format!("/v1/sessions/{ID}") {
                Reply::json(200, session_json(ID, "a", "asleep"))
            } else {
                not_found()
            }
        })
        .await;
        let client = client(&fake);
        let session = client.sessions().get(ID).await.unwrap();
        assert_eq!(session.last_info().unwrap().state, SessionState::Asleep);
        assert_eq!(session.refresh().await.unwrap().name, "a");

        // A handle asks nothing until it is used.
        let handle = client.sessions().handle("s-zzzzz").unwrap();
        assert!(handle.last_info().is_none());
        assert_eq!(fake.calls().len(), 2);
        let error = handle.refresh().await.unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::NotFound {
                message: "session not found".into()
            }
        );

        // What is not an id never reaches a URL.
        for bad in ["", "abcde", "s-../v1/me", "s-ABCDE"] {
            assert!(
                matches!(
                    client.session(bad.into()),
                    Err(ComputerUseError::Configuration { .. })
                ),
                "{bad}"
            );
        }
    }

    #[tokio::test]
    async fn renames_stops_and_resumes_with_a_patch() {
        let fake = Fake::api(|request| {
            let body = request.json();
            let state = match body["action"].as_str() {
                Some("stop") => "stopping",
                Some("resume") => "starting",
                _ => "running",
            };
            Reply::json(
                200,
                session_json(ID, body["name"].as_str().unwrap_or("a"), state),
            )
        })
        .await;
        let session = client(&fake).session(ID.into()).unwrap();

        assert_eq!(
            session.rename("renamed".into()).await.unwrap().name,
            "renamed"
        );
        assert_eq!(session.stop().await.unwrap().state, SessionState::Stopping);
        assert_eq!(
            session.resume().await.unwrap().state,
            SessionState::Starting
        );
        assert_eq!(session.last_info().unwrap().state, SessionState::Starting);

        let patches: Vec<Value> = fake
            .requests()
            .iter()
            .filter(|r| r.is("PATCH", &format!("/v1/sessions/{ID}")))
            .map(Request::json)
            .collect();
        assert_eq!(
            patches,
            [
                json!({"name": "renamed"}),
                json!({"action": "stop"}),
                json!({"action": "resume"})
            ]
        );
    }

    #[tokio::test]
    async fn sleeps_and_wakes_with_their_routes() {
        let fake = Fake::api(|request| {
            if request.path.ends_with("/sleep") {
                let mut session = session_json(ID, "a", "asleep");
                session["stateSaved"] = json!(true);
                session["stoppedBy"] = json!("sleep");
                return Reply::json(200, session);
            }
            Reply::json(200, session_json(ID, "a", "starting"))
        })
        .await;
        let session = client(&fake).session(ID.into()).unwrap();

        let asleep = session.sleep().await.unwrap();
        assert_eq!(asleep.state, SessionState::Asleep);
        assert_eq!(asleep.state_saved, Some(true));
        assert_eq!(asleep.stopped_by.as_deref(), Some("sleep"));
        assert_eq!(session.last_info().unwrap().state, SessionState::Asleep);

        let waking = session.wake().await.unwrap();
        assert_eq!(waking.state, SessionState::Starting);
        assert_eq!(waking.state_saved, None);

        assert_eq!(
            fake.calls(),
            [
                format!("POST /v1/sessions/{ID}/sleep"),
                format!("POST /v1/sessions/{ID}/wake")
            ]
        );
        assert!(fake
            .requests()
            .iter()
            .all(|r| r.path == "/oauth/token" || r.body.is_empty()));
    }

    #[tokio::test]
    async fn a_session_that_is_not_running_cannot_be_put_to_sleep() {
        let fake = Fake::api(|_| {
            Reply::json(
                409,
                json!({"error": "session is still starting; put it to sleep once it is running"}),
            )
        })
        .await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .sleep()
            .await
            .unwrap_err();
        assert!(
            matches!(error, ComputerUseError::Conflict { ref message, .. } if message.contains("still starting")),
            "{error:?}"
        );
        assert_eq!(fake.calls().len(), 1);
    }

    #[tokio::test]
    async fn a_sleep_is_allowed_longer_than_the_clients_timeout() {
        // Answers after 300 ms: the snapshot being taken.
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let slow = tokio::spawn(async move {
            use tokio::io::{AsyncReadExt, AsyncWriteExt};
            while let Ok((mut socket, _)) = listener.accept().await {
                let mut request = [0u8; 2048];
                let _ = socket.read(&mut request).await;
                tokio::time::sleep(Duration::from_millis(300)).await;
                let body = session_json(ID, "a", "asleep").to_string();
                let answer = format!(
                    "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                    body.len()
                );
                let _ = socket.write_all(answer.as_bytes()).await;
            }
        });
        let client = Client::builder()
            .api_token(TOKEN)
            .base_url(url)
            .exchange_token(false)
            .timeout(Duration::from_millis(100))
            .max_retries(0)
            .build()
            .unwrap();
        let session = client.session(ID.into()).unwrap();
        assert!(matches!(
            session.refresh().await,
            Err(ComputerUseError::Timeout { .. })
        ));
        assert_eq!(session.sleep().await.unwrap().state, SessionState::Asleep);
        slow.abort();
    }

    #[tokio::test]
    async fn deletes() {
        let fake = Fake::api(|_| Reply::empty(204)).await;
        client(&fake)
            .session(ID.into())
            .unwrap()
            .delete()
            .await
            .unwrap();
        assert_eq!(fake.calls(), [format!("DELETE /v1/sessions/{ID}")]);
    }

    #[tokio::test]
    async fn waits_until_running() {
        let polls = AtomicUsize::new(0);
        let fake = Fake::api(move |_| {
            let state = if polls.fetch_add(1, Ordering::SeqCst) == 0 {
                "starting"
            } else {
                "running"
            };
            Reply::json(200, session_json(ID, "a", state))
        })
        .await;
        let session = client(&fake).session(ID.into()).unwrap();
        let info = session.wait_until_running(Some(10_000)).await.unwrap();
        assert_eq!(info.state, SessionState::Running);
        assert_eq!(fake.calls().len(), 2);
    }

    #[tokio::test]
    async fn a_wait_ends_when_the_session_fails_or_the_time_runs_out() {
        let fake = Fake::api(|_| {
            let mut session = session_json(ID, "a", "failed");
            session["message"] = json!("image pull failed");
            Reply::json(200, session)
        })
        .await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .wait_until_running(None)
            .await
            .unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::SessionFailed {
                message: "image pull failed".into()
            }
        );

        let fake = Fake::api(|_| Reply::json(200, session_json(ID, "a", "starting"))).await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .wait_until(SessionState::Running, Some(50))
            .await
            .unwrap_err();
        assert!(
            matches!(error, ComputerUseError::Timeout { .. }),
            "{error:?}"
        );
    }
}

mod policies {
    use super::*;

    fn policy_json(state: &str, version: i64) -> Value {
        json!({
            "kind": "rego", "version": version, "hash": "h", "state": state,
            "management": {"mode": "iac", "managed_url": "https://git.example/p.rego"},
            "source": "package p", "rego": "package p",
            "errors": [], "warnings": [{"row": 1, "col": 2, "code": "w", "message": "careful"}],
            "loaded": {"replicas": 1, "total": 2},
            "updated": "2026-10-01T12:00:00Z", "updated_by": "token:ci",
        })
    }

    #[tokio::test]
    async fn reads() {
        let fake = Fake::api(|_| Reply::json(200, policy_json("ready", 3))).await;
        let policy = client(&fake)
            .session(ID.into())
            .unwrap()
            .policy()
            .await
            .unwrap();
        assert_eq!(fake.calls(), [format!("GET /v1/sessions/{ID}/policy")]);
        assert_eq!(policy.state, PolicyState::Ready);
        assert_eq!(policy.version, Some(3));
        assert_eq!(policy.management.unwrap().mode, ManagementMode::Iac);
        assert_eq!(policy.warnings[0].row, Some(1));
        assert_eq!(policy.loaded.unwrap().total, 2);
        assert_eq!(policy.updated_by.as_deref(), Some("token:ci"));
    }

    #[tokio::test]
    async fn writes_conditionally_and_reports_loading() {
        let fake = Fake::api(|_| Reply::json(202, policy_json("loading", 4))).await;
        let input = PolicyInputBuilder::new()
            .source("package p".into())
            .management(Management {
                mode: ManagementMode::Iac,
                managed_url: Some("https://git.example/p.rego".into()),
            })
            .if_match_version(3)
            .build()
            .unwrap();
        let policy = client(&fake)
            .session(ID.into())
            .unwrap()
            .put_policy(input)
            .await
            .unwrap();
        assert_eq!(policy.state, PolicyState::Loading);
        let put = fake.requests().pop().unwrap();
        assert!(put.is("PUT", &format!("/v1/sessions/{ID}/policy")));
        assert_eq!(put.header("if-match"), "\"3\"");
        assert_eq!(
            put.json(),
            json!({"kind": "rego", "source": "package p", "management": {"mode": "iac", "managed_url": "https://git.example/p.rego"}})
        );
    }

    #[tokio::test]
    async fn an_invalid_policy_carries_its_diagnostics() {
        let fake = Fake::api(|_| {
            Reply::json(422, json!({"error": "the policy does not validate",
                "errors": [{"row": 3, "col": 1, "code": "rego_parse_error", "message": "unexpected }"}], "warnings": []}))
        })
        .await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .put_policy(PolicyInput {
                source: "package p }".into(),
                management: None,
                if_match_version: None,
            })
            .await
            .unwrap_err();
        let ComputerUseError::InvalidPolicy {
            message,
            errors,
            warnings,
        } = error
        else {
            panic!("{error:?}")
        };
        assert_eq!(message, "the policy does not validate");
        assert_eq!((errors[0].row, errors[0].col), (Some(3), Some(1)));
        assert_eq!(errors[0].code, "rego_parse_error");
        assert!(warnings.is_empty());
        assert_eq!(fake.calls().len(), 1, "a 422 is not tried again");
    }

    #[tokio::test]
    async fn a_policy_managed_elsewhere_is_a_conflict_with_its_url() {
        let fake = Fake::api(|_| Reply::json(409, json!({"error": "this policy is managed in the editor", "managed_url": "https://git.example/p.rego"}))).await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .reset_policy()
            .await
            .unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::Conflict {
                message: "this policy is managed in the editor".into(),
                code: None,
                managed_url: Some("https://git.example/p.rego".into()),
                billing_url: None,
            }
        );
    }

    #[tokio::test]
    async fn a_stale_version_is_412() {
        let fake =
            Fake::api(|_| Reply::json(412, json!({"error": "the policy has changed"}))).await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .put_policy(PolicyInput {
                source: "package p".into(),
                management: None,
                if_match_version: Some(1),
            })
            .await
            .unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::Api {
                status: 412,
                message: "the policy has changed".into(),
                code: None
            }
        );
    }

    #[tokio::test]
    async fn resets_and_changes_management() {
        let fake = Fake::api(|_| Reply::json(200, policy_json("ready", 5))).await;
        let session = client(&fake).session(ID.into()).unwrap();
        session.reset_policy().await.unwrap();
        session
            .set_policy_management(Management {
                mode: ManagementMode::Editor,
                managed_url: None,
            })
            .await
            .unwrap();
        assert_eq!(
            fake.calls(),
            [
                format!("DELETE /v1/sessions/{ID}/policy"),
                format!("PUT /v1/sessions/{ID}/policy/management")
            ]
        );
        assert_eq!(
            fake.requests().pop().unwrap().json(),
            json!({"mode": "editor"})
        );
    }

    #[tokio::test]
    async fn validates_evaluates_and_lists_presets() {
        let fake = Fake::api(|request| match request.path.as_str() {
            "/v1/policies/validate" => Reply::json(200, json!({"ok": false, "errors": [{"code": "c", "message": "m"}], "warnings": []})),
            "/v1/policies/evaluate" => Reply::json(200, json!({"ok": true, "allow": false})),
            _ => Reply::json(200, json!([{"id": "unrestricted", "title": "Unrestricted", "description": "d", "kind": "rego", "source": "package p"}])),
        })
        .await;
        let client = client(&fake);

        // A list the server wrote as null is an empty list.
        let nulls: computeruse::Validation =
            serde_json::from_str(r#"{"ok": true, "errors": null, "warnings": null}"#).unwrap();
        assert!(nulls.errors.is_empty() && nulls.warnings.is_empty());

        // An invalid policy is a verdict, not an error.
        let verdict = client.validate_policy("package p }".into()).await.unwrap();
        assert!(!verdict.ok);
        assert_eq!(verdict.errors[0].message, "m");
        assert_eq!(verdict.errors[0].row, None);

        let outcome = client
            .evaluate_policy("package p".into(), r#"{"tool": "run_js"}"#.into())
            .await
            .unwrap();
        assert_eq!((outcome.ok, outcome.allow), (true, Some(false)));

        let presets = client.policy_presets().await.unwrap();
        assert_eq!(presets[0].id, "unrestricted");

        let requests = fake.requests();
        assert_eq!(
            requests[1].json(),
            json!({"kind": "rego", "source": "package p }"})
        );
        assert_eq!(
            requests[2].json(),
            json!({"kind": "rego", "source": "package p", "input": {"tool": "run_js"}})
        );
        assert_eq!(
            fake.calls(),
            [
                "POST /v1/policies/validate",
                "POST /v1/policies/evaluate",
                "GET /v1/policy-presets"
            ]
        );

        let error = client
            .evaluate_policy("package p".into(), "not json".into())
            .await
            .unwrap_err();
        assert!(matches!(error, ComputerUseError::Configuration { .. }));
    }
}

mod mcp {
    use super::*;

    const MCP_SESSION: &str = "mcp-session-1";

    /// A fake mcp-js: answers the handshake, and `tools/call` and
    /// `tools/list` with what `tool` returns for the request's params.
    fn mcp_reply(request: &Request, tool: impl Fn(&Value) -> Value) -> Reply {
        let message = request.json();
        let id = message["id"].clone();
        match message["method"].as_str().unwrap_or("") {
            "initialize" => Reply::sse(json!({"jsonrpc": "2.0", "id": id, "result": {
                "protocolVersion": "2025-03-26", "capabilities": {"tools": {}},
                "serverInfo": {"name": "mcp-js", "version": "0"}}}))
            .header("mcp-session-id", MCP_SESSION),
            "notifications/initialized" => Reply::empty(202),
            _ => {
                Reply::sse(json!({"jsonrpc": "2.0", "id": id, "result": tool(&message["params"])}))
            }
        }
    }

    fn run_js_answer(body: Value) -> Value {
        json!({"content": [{"type": "text", "text": body.to_string()}]})
    }

    #[tokio::test]
    async fn run_js_makes_the_handshake_once_and_returns_the_output() {
        let fake =
            Fake::api(|request| mcp_reply(request, |_| run_js_answer(json!({"output": "42\n"}))))
                .await;
        let session = client(&fake).session(ID.into()).unwrap();

        let result = session.run_js("console.log(6 * 7)".into()).await.unwrap();
        assert_eq!(result.output, "42\n");
        assert_eq!(result.error, None);
        assert!(result.artifacts.is_empty());
        session.run_js("console.log(1)".into()).await.unwrap();

        let requests: Vec<Request> = fake
            .requests()
            .into_iter()
            .filter(|r| r.path != "/oauth/token")
            .collect();
        let methods: Vec<String> = requests
            .iter()
            .map(|r| r.json()["method"].as_str().unwrap().to_owned())
            .collect();
        assert_eq!(
            methods,
            [
                "initialize",
                "notifications/initialized",
                "tools/call",
                "tools/call"
            ]
        );
        for request in &requests {
            assert!(request.is("POST", &format!("/{ID}/mcp")));
            assert_eq!(request.header("authorization"), "Bearer access-1");
            assert_eq!(
                request.header("accept"),
                "application/json, text/event-stream"
            );
        }
        let initialize = requests[0].json();
        assert_eq!(initialize["params"]["protocolVersion"], "2025-06-18");
        assert_eq!(
            initialize["params"]["clientInfo"]["name"],
            "computeruse-sdk"
        );
        assert_eq!(requests[0].header("mcp-session-id"), "");
        // A notification has no id.
        assert!(requests[1].json().get("id").is_none());
        // After the handshake: the server's session and the version it chose.
        for request in &requests[1..] {
            assert_eq!(request.header("mcp-session-id"), MCP_SESSION);
            assert_eq!(request.header("mcp-protocol-version"), "2025-03-26");
        }
        assert_eq!(
            requests[2].json()["params"],
            json!({"name": "run_js", "arguments": {"code": "console.log(6 * 7)"}})
        );
    }

    #[tokio::test]
    async fn run_js_passes_its_limits_and_returns_errors_and_artifacts() {
        let fake = Fake::api(|request| {
            mcp_reply(request, |_| {
                json!({"content": [
                    {"type": "text", "text": json!({"output": "before\n", "error": "ReferenceError: x is not defined", "artifacts": [{"key": "shot"}]}).to_string()},
                    {"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"},
                ]})
            })
        })
        .await;
        let session = client(&fake).session(ID.into()).unwrap();
        let request = RunJsRequestBuilder::new()
            .code("x".into())
            .heap_memory_max_mb(64)
            .execution_timeout_secs(120)
            .build()
            .unwrap();
        let result = session.run_js_with(request).await.unwrap();

        // The program failed; the call did not.
        assert_eq!(result.output, "before\n");
        assert_eq!(
            result.error.as_deref(),
            Some("ReferenceError: x is not defined")
        );
        assert_eq!(result.artifacts.len(), 1);
        assert_eq!(result.artifacts[0].kind, "image");
        assert_eq!(result.artifacts[0].mime_type.as_deref(), Some("image/png"));
        assert_eq!(result.artifacts[0].data.as_deref(), Some(&b"hello"[..]));
        assert!(result.raw_json.contains("\"artifacts\""));

        assert_eq!(
            fake.requests().pop().unwrap().json()["params"]["arguments"],
            json!({"code": "x", "heap_memory_max_mb": 64, "execution_timeout_secs": 120})
        );
    }

    #[tokio::test]
    async fn takes_a_plain_json_answer_too() {
        let fake = Fake::api(|request| {
            let message = request.json();
            match message["method"].as_str().unwrap() {
                "initialize" => Reply::json(200, json!({"jsonrpc": "2.0", "id": message["id"], "result": {"protocolVersion": "2025-06-18"}})),
                "notifications/initialized" => Reply::empty(202),
                _ => Reply::json(200, json!({"jsonrpc": "2.0", "id": message["id"], "result": run_js_answer(json!({"output": "plain"}))})),
            }
        })
        .await;
        let session = client(&fake).session(ID.into()).unwrap();
        assert_eq!(session.run_js("1".into()).await.unwrap().output, "plain");
        // No Mcp-Session-Id was given, so none is sent.
        assert_eq!(fake.requests().pop().unwrap().header("mcp-session-id"), "");
    }

    #[tokio::test]
    async fn calls_and_lists_tools() {
        let fake = Fake::api(|request| {
            mcp_reply(request, |params| {
                if params.get("name").is_some() {
                    json!({"content": [{"type": "text", "text": "one"}, {"type": "text", "text": "two"}], "structuredContent": {"n": 2}})
                } else if params.get("cursor").is_some() {
                    json!({"tools": [{"name": "list_artifacts", "inputSchema": {"type": "object"}}]})
                } else {
                    json!({"tools": [{"name": "run_js", "description": "Runs code.", "inputSchema": {"type": "object", "required": ["code"]}}], "nextCursor": "page-2"})
                }
            })
        })
        .await;
        let session = client(&fake).session(ID.into()).unwrap();

        let result = session
            .call_tool("get_artifact".into(), Some(r#"{"key": "shot"}"#.into()))
            .await
            .unwrap();
        assert_eq!(result.text(), "one\ntwo");
        assert_eq!(result.structured_json.as_deref(), Some(r#"{"n":2}"#));
        assert_eq!(
            fake.requests().pop().unwrap().json()["params"],
            json!({"name": "get_artifact", "arguments": {"key": "shot"}})
        );

        session
            .call_tool("list_artifacts".into(), None)
            .await
            .unwrap();
        assert_eq!(
            fake.requests().pop().unwrap().json()["params"]["arguments"],
            json!({})
        );

        let tools = session.list_tools().await.unwrap();
        assert_eq!(
            tools.iter().map(|t| t.name.as_str()).collect::<Vec<_>>(),
            ["run_js", "list_artifacts"]
        );
        assert_eq!(tools[0].description.as_deref(), Some("Runs code."));
        assert!(tools[0].input_schema_json.contains("required"));

        for bad in ["nope", "[1]"] {
            let error = session
                .call_tool("x".into(), Some(bad.into()))
                .await
                .unwrap_err();
            assert!(matches!(error, ComputerUseError::Configuration { .. }));
        }
    }

    #[tokio::test]
    async fn a_tool_that_reports_failure_and_a_json_rpc_error_are_errors() {
        let fake = Fake::api(|request| mcp_reply(request, |_| json!({"isError": true, "content": [{"type": "text", "text": "denied by policy"}]}))).await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .run_js("1".into())
            .await
            .unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::Tool {
                tool: "run_js".into(),
                message: "denied by policy".into()
            }
        );

        let fake = Fake::api(|request| {
            let message = request.json();
            if message["method"] == "tools/call" {
                return Reply::sse(json!({"jsonrpc": "2.0", "id": message["id"], "error": {"code": -32602, "message": "unknown tool"}}));
            }
            mcp_reply(request, |_| json!({}))
        })
        .await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .call_tool("nope".into(), None)
            .await
            .unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::Mcp {
                code: -32602,
                message: "unknown tool".into()
            }
        );
    }

    #[tokio::test]
    async fn waits_for_a_session_that_is_waking() {
        // The proxy answers 504 with Retry-After while the desktop wakes.
        let fake = Fake::start(|request, n| {
            if request.is("POST", "/oauth/token") {
                return Reply::json(200, json!({"access_token": "a", "token_type": "Bearer", "expires_in": 3600, "scope": ""}));
            }
            if n <= 6 {
                return Reply::text(504, "session is waking up; retry shortly\n").header("retry-after", "0");
            }
            mcp_reply(request, |_| run_js_answer(json!({"output": "awake"})))
        })
        .await;
        // More 504s than max_retries allows: waking has its own limit.
        let client = Client::builder()
            .api_token(TOKEN)
            .base_url(&fake.url)
            .max_retries(1)
            .build()
            .unwrap();
        let result = client
            .session(ID.into())
            .unwrap()
            .run_js("1".into())
            .await
            .unwrap();
        assert_eq!(result.output, "awake");
        assert_eq!(fake.calls().len(), 6 + 3);
    }

    #[tokio::test]
    async fn gives_up_on_a_wake_after_the_wake_timeout() {
        let fake = Fake::api(|_| {
            Reply::text(504, "session is waking up; retry shortly\n").header("retry-after", "1")
        })
        .await;
        let client = Client::builder()
            .api_token(TOKEN)
            .base_url(&fake.url)
            .wake_timeout(Duration::from_millis(100))
            .build()
            .unwrap();
        let error = client
            .session(ID.into())
            .unwrap()
            .run_js("1".into())
            .await
            .unwrap_err();
        assert!(
            matches!(error, ComputerUseError::Timeout { ref operation } if operation.contains("waking")),
            "{error:?}"
        );
        assert_eq!(fake.calls().len(), 1);
    }

    #[tokio::test]
    async fn a_stopped_session_is_a_conflict_and_a_failed_one_is_502() {
        let fake = Fake::api(|_| Reply::text(409, "session is stopped; resume it first\n")).await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .run_js("1".into())
            .await
            .unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::Conflict {
                message: "session is stopped; resume it first".into(),
                code: None,
                managed_url: None,
                billing_url: None
            }
        );
        assert_eq!(fake.calls().len(), 1, "not tried again");

        let fake = Fake::api(|_| Reply::text(502, "session is not responding\n")).await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .run_js("1".into())
            .await
            .unwrap_err();
        assert_eq!(error.status(), Some(502));
        assert_eq!(
            fake.calls().len(),
            1,
            "the call may have run: not tried again"
        );
    }

    #[tokio::test]
    async fn billing_refusal_on_the_mcp_endpoint_is_payment_required() {
        let fake = Fake::api(|_| Reply::json(402, json!({"jsonrpc": "2.0", "id": null, "error": {"code": -32002, "message": "This session is asleep because its owner is out of credit."}}))).await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .run_js("1".into())
            .await
            .unwrap_err();
        assert!(
            matches!(error, ComputerUseError::PaymentRequired { ref message, .. } if message.contains("out of credit")),
            "{error:?}"
        );
    }

    #[tokio::test]
    async fn makes_the_handshake_again_when_the_server_forgot_the_mcp_session() {
        // The desktop restarted: its mcp-js no longer knows mcp-session-1.
        let restarted = AtomicUsize::new(0);
        let fake = Fake::api(move |request| {
            let message = request.json();
            match message["method"].as_str().unwrap() {
                "initialize" => {
                    let n = restarted.fetch_add(1, Ordering::SeqCst) + 1;
                    Reply::sse(json!({"jsonrpc": "2.0", "id": message["id"], "result": {"protocolVersion": "2025-06-18"}}))
                        .header("mcp-session-id", &format!("mcp-session-{n}"))
                }
                "notifications/initialized" => Reply::empty(202),
                _ if request.header("mcp-session-id") == "mcp-session-1" && restarted.load(Ordering::SeqCst) == 1 && message["params"]["arguments"]["code"] == "second" => {
                    Reply::text(404, "Session not found")
                }
                _ => Reply::sse(json!({"jsonrpc": "2.0", "id": message["id"], "result": run_js_answer(json!({"output": request.header("mcp-session-id")}))})),
            }
        })
        .await;
        let session = client(&fake).session(ID.into()).unwrap();
        assert_eq!(
            session.run_js("first".into()).await.unwrap().output,
            "mcp-session-1"
        );
        assert_eq!(
            session.run_js("second".into()).await.unwrap().output,
            "mcp-session-2"
        );
    }

    #[tokio::test]
    async fn a_session_that_is_not_there_is_not_found() {
        let fake = Fake::api(|_| Reply::json(404, json!({"error": "session not found"}))).await;
        let error = client(&fake)
            .session(ID.into())
            .unwrap()
            .run_js("1".into())
            .await
            .unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::NotFound {
                message: "session not found".into()
            }
        );
    }
}

mod retries_and_errors {
    use super::*;

    #[tokio::test]
    async fn a_read_is_tried_again_after_503_and_429() {
        let fake = Fake::api({
            let n = AtomicUsize::new(0);
            move |_| match n.fetch_add(1, Ordering::SeqCst) {
                0 => Reply::json(503, json!({"error": "authorization unavailable"})),
                1 => Reply::json(429, json!({"error": "slow down"})).header("retry-after", "0"),
                2 => Reply::text(502, "bad gateway"),
                _ => Reply::json(200, json!([])),
            }
        })
        .await;
        assert!(client(&fake).list_sessions().await.unwrap().is_empty());
        assert_eq!(fake.calls().len(), 4);
    }

    #[tokio::test]
    async fn gives_up_after_max_retries_with_the_last_error() {
        let fake = Fake::api(|_| {
            Reply::json(429, json!({"error": "slow down"})).header("retry-after", "0")
        })
        .await;
        let client = Client::builder()
            .api_token(TOKEN)
            .base_url(&fake.url)
            .max_retries(2)
            .build()
            .unwrap();
        let error = client.me().await.unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::RateLimited {
                message: "slow down".into(),
                retry_after_secs: Some(0)
            }
        );
        assert_eq!(fake.calls().len(), 3, "one try and two more");

        let fake = Fake::api(|_| Reply::json(503, json!({"error": "down"}))).await;
        let client = Client::builder()
            .api_token(TOKEN)
            .base_url(&fake.url)
            .max_retries(0)
            .build()
            .unwrap();
        assert_eq!(client.me().await.unwrap_err().status(), Some(503));
        assert_eq!(fake.calls().len(), 1);
    }

    #[tokio::test]
    async fn a_create_is_tried_again_only_when_it_was_not_carried_out() {
        // 502: the session may exist. Trying again could make two.
        let fake = Fake::api(|_| Reply::text(502, "bad gateway")).await;
        let error = client(&fake).sessions().create().send().await.unwrap_err();
        assert_eq!(error.status(), Some(502));
        assert_eq!(fake.calls().len(), 1);

        // 503: the token could not be checked; nothing was done.
        let fake = Fake::api({
            let n = AtomicUsize::new(0);
            move |_| {
                if n.fetch_add(1, Ordering::SeqCst) == 0 {
                    Reply::json(503, json!({"error": "unavailable"}))
                } else {
                    Reply::json(201, session_json(ID, "a", "starting"))
                }
            }
        })
        .await;
        client(&fake).sessions().create().send().await.unwrap();
        assert_eq!(fake.calls().len(), 2);
    }

    #[tokio::test]
    async fn the_documented_answers_each_have_an_error() {
        let cases: Vec<(Reply, ComputerUseError)> = vec![
            (
                Reply::json(
                    402,
                    json!({"error": "Add a payment method.", "code": "payment_method_required", "billingUrl": "https://app.example/billing"}),
                ),
                ComputerUseError::PaymentRequired {
                    message: "Add a payment method.".into(),
                    code: Some("payment_method_required".into()),
                    billing_url: Some("https://app.example/billing".into()),
                },
            ),
            (
                Reply::json(
                    403,
                    json!({"error": "this token lacks the scope sessions:write"}),
                ),
                ComputerUseError::Forbidden {
                    message: "this token lacks the scope sessions:write".into(),
                    code: None,
                    billing_url: None,
                },
            ),
            (
                not_found(),
                ComputerUseError::NotFound {
                    message: "session not found".into(),
                },
            ),
            (
                Reply::json(
                    409,
                    json!({"error": "session limit reached; delete one first"}),
                ),
                ComputerUseError::Conflict {
                    message: "session limit reached; delete one first".into(),
                    code: None,
                    managed_url: None,
                    billing_url: None,
                },
            ),
            (
                Reply::json(400, json!({"error": "name must be 1 to 63 characters"})),
                ComputerUseError::Api {
                    status: 400,
                    message: "name must be 1 to 63 characters".into(),
                    code: None,
                },
            ),
            (
                Reply::json(500, json!({"error": "cluster request failed"})),
                ComputerUseError::Api {
                    status: 500,
                    message: "cluster request failed".into(),
                    code: None,
                },
            ),
        ];
        for (reply, expected) in cases {
            let answer = reply.clone();
            let fake = Fake::api(move |_| answer.clone()).await;
            let error = client(&fake)
                .sessions()
                .create()
                .name("x")
                .send()
                .await
                .unwrap_err();
            assert_eq!(error, expected);
            assert_eq!(error.status(), Some(reply.status));
            assert_eq!(fake.calls().len(), 1, "{} is not tried again", reply.status);
        }
        let payment = ComputerUseError::PaymentRequired {
            message: "m".into(),
            code: Some("out_of_credit".into()),
            billing_url: None,
        };
        assert_eq!(payment.code(), Some("out_of_credit"));
    }

    #[tokio::test]
    async fn billing_refusals_that_are_not_402_keep_their_code_and_link() {
        let fake = Fake::api(|request| {
            if request.method == "POST" {
                return Reply::json(409, json!({"error": "Your plan allows 2 sessions awake.", "code": "awake_limit", "billingUrl": "https://app.example/billing", "limit": 2}));
            }
            Reply::json(403, json!({"error": "This account is blocked.", "code": "account_blocked", "billingUrl": "https://app.example/billing"}))
        })
        .await;
        let client = client(&fake);
        let error = client.sessions().create().send().await.unwrap_err();
        assert_eq!(
            (error.status(), error.code(), error.billing_url()),
            (
                Some(409),
                Some("awake_limit"),
                Some("https://app.example/billing")
            )
        );
        let error = client.list_sessions().await.unwrap_err();
        assert_eq!(
            (error.status(), error.code(), error.billing_url()),
            (
                Some(403),
                Some("account_blocked"),
                Some("https://app.example/billing")
            )
        );
    }

    #[tokio::test]
    async fn a_success_that_is_not_the_json_expected_is_a_decode_error() {
        let fake = Fake::api(|_| Reply::text(200, "<html>a proxy's page</html>")).await;
        let error = client(&fake).me().await.unwrap_err();
        assert!(
            matches!(error, ComputerUseError::Decode { .. }),
            "{error:?}"
        );
    }

    #[tokio::test]
    async fn a_refused_connection_is_a_transport_error() {
        // A port nothing listens on.
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        drop(listener);
        let client = Client::builder()
            .api_token(TOKEN)
            .base_url(url)
            .exchange_token(false)
            .max_retries(1)
            .retry_base_delay(Duration::from_millis(1))
            .build()
            .unwrap();
        let error = client.me().await.unwrap_err();
        assert!(
            matches!(error, ComputerUseError::Transport { .. }),
            "{error:?}"
        );
        assert!(!error.to_string().contains("bjs_"));
    }

    #[tokio::test]
    async fn no_answer_within_the_timeout_is_a_timeout() {
        // Accepts and says nothing.
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let silent = tokio::spawn(async move {
            let mut held = Vec::new();
            while let Ok((socket, _)) = listener.accept().await {
                held.push(socket);
            }
        });
        let client = Client::builder()
            .api_token(TOKEN)
            .base_url(url)
            .exchange_token(false)
            .timeout(Duration::from_millis(100))
            .max_retries(0)
            .build()
            .unwrap();
        let error = client.me().await.unwrap_err();
        assert_eq!(
            error,
            ComputerUseError::Timeout {
                operation: "me".into()
            }
        );
        silent.abort();
    }
}

mod builders {
    use super::*;

    #[test]
    fn a_setter_returns_a_new_builder_and_leaves_its_receiver() {
        let base = CreateSessionRequestBuilder::new();
        let first = base.name("first".into());
        let second = base
            .name("second".into())
            .policy_preset("unrestricted".into());

        assert_eq!(base.build().unwrap(), CreateSessionRequest::default());
        assert_eq!(first.build().unwrap().name.as_deref(), Some("first"));
        let second = second.build().unwrap();
        assert_eq!(second.name.as_deref(), Some("second"));
        assert_eq!(second.policy_preset.as_deref(), Some("unrestricted"));
        assert!(second.policy.is_none());
    }

    #[test]
    fn a_required_field_left_out_is_named() {
        let error = ClientOptionsBuilder::new()
            .base_url("https://api.example.test".into())
            .build()
            .unwrap_err();
        assert_eq!(
            error,
            BuildError::MissingRequiredField {
                record_type: "ClientOptions".into(),
                field: "api_token".into()
            }
        );
        assert_eq!(
            error.to_string(),
            "ClientOptions is missing required field api_token"
        );

        let error = RunJsRequestBuilder::new().build().unwrap_err();
        assert_eq!(
            error.to_string(),
            "RunJsRequest is missing required field code"
        );
        assert_eq!(
            PolicyInputBuilder::new().build().unwrap_err().to_string(),
            "PolicyInput is missing required field source"
        );
    }

    #[tokio::test]
    async fn generated_builders_make_a_working_client() {
        let fake = Fake::api(|_| Reply::json(201, session_json(ID, "built", "starting"))).await;
        let options: ClientOptions = ClientOptions::builder()
            .api_token(TOKEN.into())
            .base_url(fake.url.clone())
            .exchange_token(false)
            .max_retries(0)
            .timeout_ms(5_000)
            .build()
            .unwrap();
        let client = Client::new(options).unwrap();
        let request = CreateSessionRequest::builder()
            .name("built".into())
            .build()
            .unwrap();
        let session = client.create_session(request).await.unwrap();
        assert_eq!(session.last_info().unwrap().name, "built");
        assert_eq!(computeruse::sdk_version(), env!("CARGO_PKG_VERSION"));
    }
}
