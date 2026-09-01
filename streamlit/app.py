"""Streamlit client for the sandbox pstad — same functionality as public/index.html,
built to answer one question: can this OCI VM host a Streamlit app in Docker
alongside the existing pstad + nginx containers, not just a static page.

Deliberately a single flat file with inline HTTP calls, no package structure —
this project's whole point is staying small and dependency-free (see the repo
README's "Provenance" section), and two zero-input workflows don't need more
than that.
"""
from __future__ import annotations

import time

import requests
import streamlit as st

st.set_page_config(page_title="Sandbox runner (Streamlit)", page_icon="🦅", layout="centered")

_TERMINAL = {"succeeded", "failed", "cancelled", "timed_out", "errored"}
_DEFAULTS = {
    "server_url": "http://127.0.0.1:8090",
    "token": "sandbox-dev-token",
    "run_id": None,
    "workflow_id": None,
    "log_lines": [],
    "log_seq": 0,
}
for key, default in _DEFAULTS.items():
    if key not in st.session_state:
        st.session_state[key] = default


def headers() -> dict:
    return {"Authorization": f"Bearer {st.session_state.token}"}


def base_url() -> str:
    return st.session_state.server_url.rstrip("/")


def submit(workflow_id: str) -> None:
    # Real API contract: multipart/form-data with a "config" JSON part — same
    # as public/index.html's submit(), even though these workflows take no
    # file inputs. trace: true so a trace.zip is always available to download.
    files = {
        "config": (
            "config.json",
            f'{{"workflowId":"{workflow_id}","env":"TST","submittedBy":"streamlit-sandbox-ui","trace":true}}',
            "application/json",
        )
    }
    resp = requests.post(f"{base_url()}/v1/runs", headers=headers(), files=files, timeout=15)
    if not resp.ok:
        st.error(f"Failed to submit: {resp.text}")
        return
    run = resp.json()
    st.session_state.run_id = run["id"]
    st.session_state.workflow_id = workflow_id
    st.session_state.log_lines = []
    st.session_state.log_seq = 0


st.title("🦅 Sandbox runner — Streamlit")
st.caption(
    "Same functionality as the static `public/index.html` UI, driving the same "
    "`pstad` API — built to prove Streamlit itself can be Dockerized and hosted "
    "on the OCI VM, not just a static page."
)

with st.sidebar:
    st.header("Connection")
    st.text_input("pstad server URL", key="server_url")
    st.text_input("Access token", key="token", type="password")

col1, col2 = st.columns(2)
with col1:
    if st.button("▶ Run Eagle workflow", use_container_width=True, disabled=bool(st.session_state.run_id)):
        submit("sandbox.eagle")
        st.rerun()
with col2:
    if st.button("▶ Run Condor workflow", use_container_width=True, disabled=bool(st.session_state.run_id)):
        submit("sandbox.condor")
        st.rerun()

if st.session_state.run_id:
    run_id = st.session_state.run_id
    try:
        run = requests.get(f"{base_url()}/v1/runs/{run_id}", headers=headers(), timeout=10).json()
    except requests.exceptions.RequestException as exc:
        st.error(f"Lost contact with pstad: {exc}")
        run = {"state": "errored"}

    state = run.get("state", "unknown")
    st.subheader(f"{st.session_state.workflow_id} — `{run_id}`")
    st.write(f"**State:** {state}")

    try:
        log = requests.get(
            f"{base_url()}/v1/runs/{run_id}/log",
            headers=headers(),
            params={"from": st.session_state.log_seq},
            timeout=10,
        ).json()
        for line in log.get("lines", []):
            st.session_state.log_lines.append(line["text"])
        st.session_state.log_seq = log.get("nextSeq", st.session_state.log_seq)
    except requests.exceptions.RequestException:
        pass

    st.code("\n".join(st.session_state.log_lines[-150:]) or "(no output yet)", language=None)

    if state not in _TERMINAL:
        time.sleep(1)
        st.rerun()
    else:
        icon = "✅" if state == "succeeded" else "❌"
        st.write(f"{icon} Finished: **{state}**")

        artifacts = run.get("artifacts") or []
        if artifacts:
            st.subheader("Artifacts")
            for a in artifacts:
                try:
                    content = requests.get(
                        f"{base_url()}/v1/runs/{run_id}/artifacts/{a['id']}",
                        headers=headers(),
                        timeout=30,
                    ).content
                    st.download_button(
                        f"⬇ {a['name']} ({round(a['size'] / 1024)} KB)",
                        data=content,
                        file_name=a["name"],
                        key=f"artifact_{a['id']}",
                    )
                except requests.exceptions.RequestException as exc:
                    st.warning(f"Could not fetch {a['name']}: {exc}")

        if st.button("Start another run"):
            st.session_state.run_id = None
            st.session_state.workflow_id = None
            st.rerun()
