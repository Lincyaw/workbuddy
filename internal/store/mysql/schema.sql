-- workbuddy MySQL schema (REQ-136 / issue #316).
--
-- Feature-parallel with the SQLite DDL produced by sqliteStore.createTables.
-- The TestSchemaParity test in internal/store cross-checks the two by
-- comparing the canonical column lists table-by-table. When you add a
-- column on one side, add it here too (or vice-versa) and the test will
-- keep you honest.
--
-- Conventions:
--   * Engine = InnoDB, charset = utf8mb4 — required for foreign keys and
--     proper unicode in issue bodies / payloads.
--   * Indexed string columns use VARCHAR(255). Bodies and JSON blobs
--     remain TEXT / LONGTEXT.
--   * AUTOINCREMENT → AUTO_INCREMENT.
--   * CURRENT_TIMESTAMP is supported on DATETIME with explicit DEFAULT.

CREATE TABLE IF NOT EXISTS events (
    id        BIGINT       NOT NULL AUTO_INCREMENT,
    ts        DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    type      VARCHAR(128) NOT NULL,
    repo      VARCHAR(255) NOT NULL,
    issue_num INT          DEFAULT NULL,
    payload   LONGTEXT,
    PRIMARY KEY (id),
    KEY idx_events_repo_issue (repo, issue_num),
    KEY idx_events_type (type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS task_queue (
    id                   VARCHAR(64)  NOT NULL,
    repo                 VARCHAR(255) NOT NULL,
    issue_num            INT          NOT NULL,
    agent_name           VARCHAR(128) NOT NULL,
    role                 VARCHAR(64)  NOT NULL DEFAULT '',
    runtime              VARCHAR(64)  NOT NULL DEFAULT '',
    workflow             VARCHAR(128) NOT NULL DEFAULT '',
    state                VARCHAR(128) NOT NULL DEFAULT '',
    worker_id            VARCHAR(128) DEFAULT NULL,
    claim_token          VARCHAR(64)  DEFAULT NULL,
    status               VARCHAR(32)  NOT NULL DEFAULT 'pending',
    lease_expires_at     DATETIME(6)  DEFAULT NULL,
    acked_at             DATETIME(6)  DEFAULT NULL,
    heartbeat_at         DATETIME(6)  DEFAULT NULL,
    completed_at         DATETIME(6)  DEFAULT NULL,
    exit_code            INT          NOT NULL DEFAULT 0,
    session_refs         LONGTEXT     NOT NULL,
    rollout_index        INT          NOT NULL DEFAULT 0,
    rollouts_total       INT          NOT NULL DEFAULT 1,
    rollout_group_id     VARCHAR(128) NOT NULL DEFAULT '',
    supervisor_agent_id  VARCHAR(128) DEFAULT NULL,
    created_at           DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at           DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY idx_task_queue_repo_issue (repo, issue_num),
    KEY idx_task_queue_status (status),
    KEY idx_task_queue_rollout (rollout_group_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workers (
    id                VARCHAR(128) NOT NULL,
    repo              VARCHAR(255) NOT NULL,
    repos_json        LONGTEXT     NOT NULL,
    roles             VARCHAR(255) NOT NULL,
    runtime           VARCHAR(64)  NOT NULL DEFAULT '',
    hostname          VARCHAR(255) DEFAULT NULL,
    mgmt_base_url     VARCHAR(512) NOT NULL DEFAULT '',
    audit_url         VARCHAR(512) NOT NULL DEFAULT '',
    tunnel            INT          NOT NULL DEFAULT 0,
    status            VARCHAR(32)  NOT NULL DEFAULT 'online',
    token_kid         VARCHAR(64)  DEFAULT NULL,
    token_hash        VARCHAR(128) DEFAULT NULL,
    token_revoked_at  DATETIME(6)  DEFAULT NULL,
    last_heartbeat    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    registered_at     DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY idx_workers_repo (repo),
    KEY idx_workers_token_kid (token_kid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS repo_registrations (
    repo          VARCHAR(255) NOT NULL,
    environment   VARCHAR(64)  NOT NULL DEFAULT '',
    status        VARCHAR(32)  NOT NULL DEFAULT 'active',
    config_json   LONGTEXT     NOT NULL,
    registered_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (repo)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS transition_counts (
    repo       VARCHAR(255) NOT NULL,
    issue_num  INT          NOT NULL,
    from_state VARCHAR(128) NOT NULL,
    to_state   VARCHAR(128) NOT NULL,
    count      INT          NOT NULL DEFAULT 0,
    PRIMARY KEY (repo, issue_num, from_state, to_state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workflow_instances (
    id            VARCHAR(64)  NOT NULL,
    workflow_name VARCHAR(128) NOT NULL,
    repo          VARCHAR(255) NOT NULL,
    issue_num     INT          NOT NULL,
    current_state VARCHAR(128) NOT NULL DEFAULT '',
    created_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uk_workflow_instances_repo_issue_wf (repo, issue_num, workflow_name),
    KEY idx_workflow_instances_repo_issue (repo, issue_num)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS workflow_transitions (
    id                   BIGINT       NOT NULL AUTO_INCREMENT,
    workflow_instance_id VARCHAR(64)  NOT NULL,
    from_state           VARCHAR(128) NOT NULL,
    to_state             VARCHAR(128) NOT NULL,
    trigger_agent        VARCHAR(128) NOT NULL DEFAULT '',
    created_at           DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY idx_workflow_transitions_instance (workflow_instance_id),
    CONSTRAINT fk_workflow_transitions_instance
        FOREIGN KEY (workflow_instance_id) REFERENCES workflow_instances(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS issue_cache (
    repo       VARCHAR(255) NOT NULL,
    issue_num  INT          NOT NULL,
    labels     LONGTEXT,
    body       LONGTEXT     NOT NULL,
    state      VARCHAR(64)  DEFAULT NULL,
    root_trace_id VARCHAR(32) NOT NULL DEFAULT '',
    parent_issue_num INT NOT NULL DEFAULT 0,
    updated_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (repo, issue_num)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS agent_sessions (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    session_id VARCHAR(128) NOT NULL,
    task_id    VARCHAR(64)  DEFAULT NULL,
    repo       VARCHAR(255) NOT NULL,
    issue_num  INT          NOT NULL,
    agent_name VARCHAR(128) NOT NULL,
    summary    LONGTEXT,
    raw_path   VARCHAR(512),
    created_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY idx_agent_sessions_repo_issue (repo, issue_num)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS sessions (
    id              BIGINT       NOT NULL AUTO_INCREMENT,
    session_id      VARCHAR(128) NOT NULL,
    task_id         VARCHAR(64)  DEFAULT NULL,
    repo            VARCHAR(255) NOT NULL,
    issue_num       INT          NOT NULL,
    agent_name      VARCHAR(128) NOT NULL,
    runtime         VARCHAR(64)  NOT NULL DEFAULT '',
    worker_id       VARCHAR(128) NOT NULL DEFAULT '',
    attempt         INT          NOT NULL DEFAULT 0,
    status          VARCHAR(32)  NOT NULL DEFAULT 'pending',
    dir             VARCHAR(512) NOT NULL DEFAULT '',
    stdout_path     VARCHAR(512) NOT NULL DEFAULT '',
    stderr_path     VARCHAR(512) NOT NULL DEFAULT '',
    tool_calls_path VARCHAR(512) NOT NULL DEFAULT '',
    metadata_path   VARCHAR(512) NOT NULL DEFAULT '',
    summary         LONGTEXT     NOT NULL,
    raw_path        VARCHAR(512) NOT NULL DEFAULT '',
    created_at      DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    closed_at       DATETIME(6)  DEFAULT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_sessions_session_id (session_id),
    KEY idx_sessions_repo_issue (repo, issue_num, id),
    KEY idx_sessions_agent (agent_name, id),
    KEY idx_sessions_worker_status (worker_id, status, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS session_routes (
    session_id VARCHAR(128) NOT NULL,
    worker_id  VARCHAR(128) NOT NULL,
    repo       VARCHAR(255) NOT NULL,
    issue_num  INT          NOT NULL,
    created_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (session_id),
    KEY idx_session_routes_worker (worker_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS issue_dependencies (
    repo                 VARCHAR(255) NOT NULL,
    issue_num            INT          NOT NULL,
    depends_on_repo      VARCHAR(255) NOT NULL,
    depends_on_issue_num INT          NOT NULL,
    source_hash          VARCHAR(128) NOT NULL,
    status               VARCHAR(32)  NOT NULL,
    PRIMARY KEY (repo, issue_num, depends_on_repo, depends_on_issue_num)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS issue_dependency_state (
    repo                  VARCHAR(255) NOT NULL,
    issue_num             INT          NOT NULL,
    verdict               VARCHAR(64)  NOT NULL,
    resume_label          VARCHAR(128) DEFAULT NULL,
    blocked_reason_hash   VARCHAR(128) DEFAULT NULL,
    override_active       INT          NOT NULL DEFAULT 0,
    graph_version         INT          NOT NULL DEFAULT 0,
    last_reaction_blocked INT          NOT NULL DEFAULT 0,
    last_evaluated_at     DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (repo, issue_num)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS issue_claim (
    repo        VARCHAR(255) NOT NULL,
    issue_num   INT          NOT NULL,
    worker_id   VARCHAR(128) NOT NULL,
    claim_token VARCHAR(64)  NOT NULL,
    acquired_at DATETIME(6)  NOT NULL,
    expires_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (repo, issue_num)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS issue_pipeline_hazards (
    repo        VARCHAR(255) NOT NULL,
    issue_num   INT          NOT NULL,
    kind        VARCHAR(64)  NOT NULL,
    fingerprint VARCHAR(128) NOT NULL,
    detected_at DATETIME(6)  NOT NULL,
    PRIMARY KEY (repo, issue_num)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS issue_cycle_state (
    repo                   VARCHAR(255) NOT NULL,
    issue_num              INT          NOT NULL,
    dev_review_cycle_count INT          NOT NULL DEFAULT 0,
    synth_cycle_count      INT          NOT NULL DEFAULT 0,
    first_dispatch_at      DATETIME(6)  DEFAULT NULL,
    cap_hit_at             DATETIME(6)  DEFAULT NULL,
    synth_cap_hit_at       DATETIME(6)  DEFAULT NULL,
    updated_at             DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (repo, issue_num)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
