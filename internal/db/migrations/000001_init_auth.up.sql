CREATE TABLE users (
    id CHAR(36) NOT NULL,
    email VARCHAR(255) NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    display_name VARCHAR(255) NOT NULL,
    role VARCHAR(32) NOT NULL DEFAULT 'member',
    is_active TINYINT(1) NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_users_email (email),
    CONSTRAINT chk_users_role CHECK (role IN ('admin', 'member'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE refresh_tokens (
    id CHAR(36) NOT NULL,
    user_id CHAR(36) NOT NULL,
    token_hash CHAR(64) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    revoked_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_refresh_tokens_token_hash (token_hash),
    KEY idx_refresh_tokens_user_id (user_id),
    KEY idx_refresh_tokens_expires_at (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC;
