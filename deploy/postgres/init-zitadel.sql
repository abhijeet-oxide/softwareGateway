-- ZITADEL gets its OWN database in the SAME instance. It creates nine schemas
-- and claims `public`; sharing one database would put two migration systems in
-- one namespace. See docs/design/24 section 3.1.
CREATE DATABASE zitadel;
