package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/pbkdf2"
)

// pgquire's read-only setting is a guard rail: SQL can switch it off. A login without write
// privileges is the real thing. roleInfo says which one a connection has; POST
// /api/profiles/{name}/readonly-role creates such a login and can switch the connection to it.

type roleInfo struct {
	User       string `json:"user"`
	Superuser  bool   `json:"superuser"`
	CreateRole bool   `json:"createRole"` // may create logins (with the superuser flag, can run the fix itself)
	WriteData  bool   `json:"writeData"`  // INSERT/UPDATE/DELETE/TRUNCATE on some table
	CreateObj  bool   `json:"createObjects"`
	Loopback   bool   `json:"loopback"` // dblink / postgres_fdw installed: writes could go through another connection
	Writable   bool   `json:"writable"` // any of the above that lets it change data
}

const roleQuery = `select current_user::text, r.rolsuper, r.rolcreaterole,
  exists (select 1 from pg_class c join pg_namespace n on n.oid = c.relnamespace
    where c.relkind in ('r', 'p') and n.nspname !~ '^pg_' and n.nspname <> 'information_schema'
      and (has_table_privilege(c.oid, 'INSERT') or has_table_privilege(c.oid, 'UPDATE')
        or has_table_privilege(c.oid, 'DELETE') or has_table_privilege(c.oid, 'TRUNCATE'))),
  exists (select 1 from pg_namespace n where n.nspname !~ '^pg_' and n.nspname <> 'information_schema'
    and has_schema_privilege(n.oid, 'CREATE')),
  exists (select 1 from pg_extension where extname in ('dblink', 'postgres_fdw'))
from pg_roles r where r.rolname = current_user`

func checkRole(ctx context.Context, conn *pgx.Conn) (roleInfo, error) {
	var ri roleInfo
	err := conn.QueryRow(ctx, roleQuery).Scan(&ri.User, &ri.Superuser, &ri.CreateRole, &ri.WriteData, &ri.CreateObj, &ri.Loopback)
	ri.Writable = ri.Superuser || ri.WriteData || ri.CreateObj
	return ri, err
}

// scramVerifier hashes a password the way Postgres stores it, so CREATE ROLE never carries it in
// plain text (statement logs included).
func scramVerifier(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	const iter = 4096
	salted := pbkdf2.Key([]byte(password), salt, iter, 32, sha256.New)
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac(salted, "Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iter, b64(salt), b64(stored[:]), b64(mac(salted, "Server Key")))
}

var roleName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// readOnlyRolePlan is the SQL that creates the login; grantAll is tried first, perSchema if it fails
// (before PostgreSQL 14, or without the right to grant pg_read_all_data).
func readOnlyRolePlan(role, database, password string, schemas []string, v14 bool) (create, grantAll, perSchema []string) {
	r := pgx.Identifier{role}.Sanitize()
	grantAll, perSchema = []string{}, []string{} // never nil: the page reads them as lists, and nil goes out as JSON null
	create = []string{
		fmt.Sprintf("create role %s login password %s nosuperuser nocreatedb nocreaterole noreplication nobypassrls inherit", r, password),
		fmt.Sprintf("alter role %s set default_transaction_read_only = on", r),
		fmt.Sprintf("grant connect on database %s to %s", pgx.Identifier{database}.Sanitize(), r),
	}
	if v14 {
		grantAll = []string{fmt.Sprintf("grant pg_read_all_data to %s", r)}
	}
	for _, s := range schemas {
		q := pgx.Identifier{s}.Sanitize()
		perSchema = append(perSchema,
			fmt.Sprintf("grant usage on schema %s to %s", q, r),
			fmt.Sprintf("grant select on all tables in schema %s to %s", q, r),
			fmt.Sprintf("grant select on all sequences in schema %s to %s", q, r),
			fmt.Sprintf("alter default privileges in schema %s grant select on tables to %s", q, r))
	}
	return
}

func (s *server) readOnlyRole(w http.ResponseWriter, r *http.Request) {
	p := s.cfg.profiles.get(r.PathValue("name"))
	if p == nil {
		writeErr(w, http.StatusNotFound, "no such connection")
		return
	}
	var in struct {
		Role     string `json:"role"`
		Database string `json:"database"`
		Switch   bool   `json:"switch"`
		DryRun   bool   `json:"dryRun"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !roleName.MatchString(in.Role) {
		writeErr(w, http.StatusBadRequest, "use letters, digits and _ for the login name (starting with a letter or _)")
		return
	}
	cfg, err := s.sessions.connConfig(p, in.Database)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A deliberate admin step, confirmed in the page: this one connection may write.
	cfg.RuntimeParams["default_transaction_read_only"] = "off"
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}
	defer conn.Close(context.Background())

	var ver int
	var db string
	var exists, prior bool
	if err := conn.QueryRow(ctx, `select current_setting('server_version_num')::int, current_database(),
		exists (select 1 from pg_roles where rolname = $1), exists (select 1 from pg_roles where rolname = $2)`,
		in.Role, p.CreatedRole).Scan(&ver, &db, &exists, &prior); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}
	// The connection remembers one created login (so forgetting it can drop it). While that one is still
	// there it's reused — a new password, then switch — since a second would leave it behind with a
	// password nobody knows.
	reuse := prior && in.Role == p.CreatedRole
	if prior && !reuse && !in.DryRun {
		writeErr(w, http.StatusConflict, fmt.Sprintf("this connection already has %s, a login pgquire created: switch to that one, or remove it first (forget the connection, or run %s)", p.CreatedRole, strings.Join(dropRoleSQL(p.CreatedRole), "; ")))
		return
	}
	rows, _ := conn.Query(ctx, `select n.nspname from pg_namespace n where n.nspname !~ '^pg_' and n.nspname <> 'information_schema'
		and not exists (select 1 from pg_depend d where d.objid = n.oid and d.deptype = 'e') order by 1`)
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}
	who, err := checkRole(ctx, conn)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}

	if in.DryRun { // the SQL to show, with a placeholder where the password goes
		create, all, per := readOnlyRolePlan(in.Role, db, "'<choose a password>'", schemas, ver >= 140000)
		if reuse {
			create, all, per = []string{rolePasswordSQL(in.Role, "'<choose a password>'")}, []string{}, []string{}
		}
		created := ""
		if prior {
			created = p.CreatedRole
		}
		writeJSON(w, http.StatusOK, map[string]any{"create": create, "grantAll": all, "perSchema": per, "exists": exists,
			"created": created, "reuse": reuse, "canCreate": who.Superuser || who.CreateRole, "user": who.User, "database": db})
		return
	}
	if reuse {
		if !in.Switch {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("pgquire already created %s for this connection — switch to it instead", in.Role))
			return
		}
		password := randomHex(24)
		if _, err := conn.Exec(ctx, rolePasswordSQL(in.Role, quoteLit(scramVerifier(password)))); err != nil {
			writeRoleErr(w, err)
			return
		}
		s.switchLogin(w, p, in.Role, db, password, true, map[string]any{"role": in.Role, "reused": true, "schemas": schemas,
			"database": db, "switched": true, "removeSQL": dropRoleSQL(in.Role)})
		return
	}
	if exists {
		writeErr(w, http.StatusConflict, fmt.Sprintf("a login called %q already exists — pick another name", in.Role))
		return
	}

	password := randomHex(24)
	create, all, per := readOnlyRolePlan(in.Role, db, quoteLit(scramVerifier(password)), schemas, ver >= 140000)
	pc := conn.PgConn()
	run := func(stmts []string) error {
		for _, q := range stmts {
			if _, err := pc.Exec(ctx, q).ReadAll(); err != nil {
				return err
			}
		}
		return nil
	}
	grants := "pg_read_all_data"
	err = run([]string{"begin"})
	if err == nil {
		err = run(create)
	}
	if err == nil {
		if len(all) == 0 || run(append([]string{"savepoint grant_all"}, all...)) != nil {
			grants = "schemas"
			if len(all) > 0 {
				run([]string{"rollback to savepoint grant_all"})
			}
			err = run(per)
		}
	}
	if err == nil {
		err = run([]string{"commit"})
	}
	if err != nil {
		pc.Exec(context.Background(), "rollback").ReadAll()
		writeRoleErr(w, err)
		return
	}
	s.switchLogin(w, p, in.Role, db, password, in.Switch, map[string]any{"role": in.Role, "grants": grants, "schemas": schemas,
		"database": db, "switched": in.Switch, "removeSQL": dropRoleSQL(in.Role)})
}

// switchLogin remembers the login on the connection (so forgetting it can offer to remove the login
// too), switches the connection to it if asked, and replies with out.
func (s *server) switchLogin(w http.ResponseWriter, p *Profile, role, db, password string, switchTo bool, out map[string]any) {
	np := *p // copy: sessions may be reading the current one
	np.CreatedRole, np.CreatedRoleDB = role, db
	if switchTo {
		np.User, np.Password = role, password
	}
	if err := s.cfg.profiles.put(&np); err != nil {
		writeErr(w, http.StatusInternalServerError, "set up the login, but couldn't save the connection: "+err.Error())
		return
	}
	if switchTo {
		s.sessions.closeProfile(p.Name) // open tabs reconnect with the new login
	}
	writeJSON(w, http.StatusOK, out)
}

func writeRoleErr(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "42501" { // insufficient_privilege
		code = http.StatusForbidden
	}
	writeJSON(w, code, map[string]any{"error": pgErrorJSON(err)})
}

// rolePasswordSQL gives a login pgquire created a new password (a SCRAM verifier, or a placeholder).
func rolePasswordSQL(role, password string) string {
	return "alter role " + pgx.Identifier{role}.Sanitize() + " password " + password
}

// dropRoleSQL removes a login pgquire created: DROP OWNED BY revokes its grants (in that database and
// on shared objects), then the role goes.
func dropRoleSQL(role string) []string {
	r := pgx.Identifier{role}.Sanitize()
	return []string{"drop owned by " + r, "drop role " + r}
}

// dropCreatedRole removes p.CreatedRole using the connection's own login — not the read-only one it
// may have been switched to, which couldn't drop itself.
func (s *server) dropCreatedRole(ctx context.Context, p *Profile) error {
	orig := *p
	orig.User, orig.Password = "", ""
	cfg, err := s.sessions.connConfig(&orig, p.CreatedRoleDB)
	if err != nil {
		return err
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "off" // a deliberate step the user confirmed
	s.sessions.closeProfile(p.Name)                            // nothing of ours stays logged in as that role
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(ctx, "select exists (select 1 from pg_roles where rolname = $1)", p.CreatedRole).Scan(&exists); err != nil || !exists {
		return err // already gone: nothing to do
	}
	for _, q := range dropRoleSQL(p.CreatedRole) {
		if _, err := conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
