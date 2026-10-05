CREATE TYPE seat_state AS ENUM ('available', 'held', 'confirmed');

CREATE TABLE shows (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
    per_user_limit INTEGER NOT NULL DEFAULT 4 CHECK (per_user_limit > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE seats (
    show_id UUID NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    seat_number TEXT NOT NULL CHECK (length(seat_number) BETWEEN 1 AND 32),
    state seat_state NOT NULL DEFAULT 'available',
    PRIMARY KEY (show_id, seat_number)
);

CREATE INDEX seats_show_state_idx
ON seats (show_id, state);
