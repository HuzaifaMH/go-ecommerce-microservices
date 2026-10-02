-- Database per service (ADR-006). Local development only; production uses separate instances.
-- Each service gets its own database and role, so cross-service access is impossible by design.
CREATE ROLE inventory LOGIN PASSWORD 'inventory';
CREATE ROLE orders LOGIN PASSWORD 'orders';
CREATE ROLE payments LOGIN PASSWORD 'payments';
CREATE ROLE notifications LOGIN PASSWORD 'notifications';

CREATE DATABASE inventory OWNER inventory;
CREATE DATABASE orders OWNER orders;
CREATE DATABASE payments OWNER payments;
CREATE DATABASE notifications OWNER notifications;
