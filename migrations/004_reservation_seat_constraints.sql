ALTER TABLE reservations
ADD CONSTRAINT reservations_id_show_unique UNIQUE (id, show_id);

ALTER TABLE reservation_seats
ADD COLUMN show_id UUID,
ADD COLUMN active BOOLEAN;

UPDATE reservation_seats AS reservation_seat
SET
    show_id = reservation.show_id,
    active = reservation.status = 'confirmed'
FROM reservations AS reservation
WHERE reservation.id = reservation_seat.reservation_id;

ALTER TABLE reservation_seats
ALTER COLUMN show_id SET NOT NULL,
ALTER COLUMN active SET NOT NULL,
ALTER COLUMN active SET DEFAULT true;

ALTER TABLE reservation_seats
ADD CONSTRAINT reservation_seats_reservation_show_fk
FOREIGN KEY (reservation_id, show_id)
REFERENCES reservations (id, show_id);

ALTER TABLE reservation_seats
ADD CONSTRAINT reservation_seats_show_seat_fk
FOREIGN KEY (show_id, seat_number)
REFERENCES seats (show_id, seat_number);

CREATE UNIQUE INDEX reservation_seats_active_show_seat_unique
ON reservation_seats (show_id, seat_number)
WHERE active;
