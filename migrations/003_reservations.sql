CREATE TYPE reservation_status AS ENUM ('confirmed', 'cancelled');

ALTER TABLE seats
DROP COLUMN IF EXISTS position;

CREATE TABLE reservations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    show_id UUID NOT NULL REFERENCES shows(id),
    user_id UUID NOT NULL REFERENCES users(id),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    amount_paise BIGINT NOT NULL CHECK (amount_paise >= 0),
    status reservation_status NOT NULL DEFAULT 'confirmed',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key)
);

CREATE TABLE reservation_seats (
    reservation_id UUID NOT NULL REFERENCES reservations(id),
    seat_number TEXT NOT NULL,
    PRIMARY KEY (reservation_id, seat_number)
);

CREATE INDEX reservations_user_show_active_idx
ON reservations (user_id, show_id)
WHERE status = 'confirmed';
