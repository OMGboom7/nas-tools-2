package database

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoricalTypesRetainSchemaRowsAndSequenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db := userSchemaFixture(t, path, `
		CREATE TABLE "Custom_Words" (
			ID INTEGER PRIMARY KEY AUTOINCREMENT,
			"OFFSET" /* legacy type */ INTEGER,
			extra BLOB, spelling TEXT COLLATE NOCASE DEFAULT 'OFFSET INTEGER,)',
			label TEXT GENERATED ALWAYS AS (spelling || ':' || "OFFSET") STORED,
			CHECK(length(spelling)>0));
		CREATE TABLE uses_word(word_id REFERENCES Custom_Words(ID) ON DELETE CASCADE);
		CREATE TABLE audits(message TEXT);
		CREATE VIEW word_view AS SELECT ID, label FROM "Custom_Words";
		CREATE UNIQUE INDEX word_spelling ON Custom_Words(lower(spelling)) WHERE "OFFSET" IS NOT NULL;
		CREATE TRIGGER "track;word" AFTER UPDATE OF "OFFSET" ON Custom_Words BEGIN
			INSERT INTO audits VALUES('field; END; OFFSET INTEGER');
			INSERT INTO audits VALUES(CASE WHEN NEW."OFFSET" IS NULL THEN 'empty' ELSE NEW.label END);
		END;
		INSERT INTO Custom_Words(ID,"OFFSET",extra,spelling) VALUES(7,-2,x'00ff','Title'),(11,'2,5',NULL,'Range'),(12,1.5,NULL,'Decimal'),(93,NULL,NULL,'deleted');
		DELETE FROM Custom_Words WHERE ID=93;
		INSERT INTO uses_word VALUES(7);
		CREATE TABLE SITE_BRUSH_TASK(
			key TEXT PRIMARY KEY, [DOWNLOAD_COUNT] TEXT NOT NULL, "REMOVE_COUNT" TEXT,
			UPLOAD_SIZE TEXT, DOWNLOAD_SIZE TEXT,
			"log,field" BLOB, "arbitrary)name" TEXT DEFAULT 'X;Y') WITHOUT ROWID;
		INSERT INTO SITE_BRUSH_TASK(key,DOWNLOAD_COUNT,REMOVE_COUNT,UPLOAD_SIZE,DOWNLOAD_SIZE,"log,field")
		VALUES ('a','  +002 ',NULL,'9223372036854775807','0',x'ffff');
	`)
	for i := 0; i < 2; i++ {
		if err := PrepareNativeUserDatabase(path); err != nil {
			t.Fatal(err)
		}
	}
	requireSchemaValue(t, db, "SELECT name FROM sqlite_schema WHERE name='Custom_Words'", "Custom_Words")
	requireSchemaValue(t, db, "SELECT type FROM pragma_table_info('Custom_Words') WHERE name='OFFSET'", "TEXT")
	requireSchemaValue(t, db, `SELECT typeof("OFFSET")||':'||"OFFSET"||':'||hex(extra) FROM Custom_Words WHERE ID=7`, "text:-2:00FF")
	requireSchemaValue(t, db, `SELECT "OFFSET" FROM Custom_Words WHERE ID=11`, "2,5")
	requireSchemaValue(t, db, `SELECT label FROM word_view WHERE ID=12`, "Decimal:1.5")
	requireSchemaValue(t, db, "SELECT COUNT(*) FROM audits", "0")
	requireSchemaValue(t, db, "SELECT word_id FROM uses_word", "7")
	requireSchemaValue(t, db, "SELECT count(*) FROM pragma_foreign_key_check", "0")
	requireSchemaValue(t, db, "SELECT count(*) FROM pragma_table_info('SITE_BRUSH_TASK') WHERE name IN ('REMOVE_COUNT','DOWNLOAD_COUNT','UPLOAD_SIZE','DOWNLOAD_SIZE') AND type='INTEGER'", "4")
	requireSchemaValue(t, db, `SELECT typeof(DOWNLOAD_COUNT)||':'||DOWNLOAD_COUNT||':'||typeof(REMOVE_COUNT)||':'||UPLOAD_SIZE||':'||hex("log,field") FROM SITE_BRUSH_TASK`, "integer:2:null:9223372036854775807:FFFF")
	requireSchemaValue(t, db, "SELECT wr FROM pragma_table_list WHERE name='SITE_BRUSH_TASK'", "1")
	if _, err := db.Exec(`UPDATE Custom_Words SET "OFFSET"='005' WHERE ID=7; INSERT INTO Custom_Words(spelling) VALUES('new')`); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT max(ID) FROM Custom_Words", "94")
	requireSchemaValue(t, db, "SELECT message FROM audits ORDER BY rowid DESC LIMIT 1", "Title:005")
	if _, err := db.Exec(`INSERT INTO Custom_Words(spelling,"OFFSET") VALUES('tItLe','01')`); err == nil {
		t.Fatal("partial expression index was lost")
	}
	backup := userSchemaFixture(t, path+MigrationBackupSuffix, "")
	requireSchemaValue(t, backup, `SELECT typeof("OFFSET") FROM Custom_Words WHERE ID=7`, "integer")
}

func TestHistoricalTypesRetainImplicitRowIDAndEmptySequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db := userSchemaFixture(t, path, `
	CREATE TABLE CUSTOM_WORDS (name TEXT,"OFFSET" INTEGER);
	INSERT INTO CUSTOM_WORDS(rowid,name,"OFFSET") VALUES(392,'fixture',3);
	CREATE TABLE SITE_BRUSH_TASK(ID INTEGER PRIMARY KEY AUTOINCREMENT,DOWNLOAD_COUNT TEXT);
	INSERT INTO SITE_BRUSH_TASK(ID) VALUES(59); DELETE FROM SITE_BRUSH_TASK;`)
	if err := PrepareNativeUserDatabase(path); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT rowid FROM CUSTOM_WORDS", "392")
	if _, err := db.Exec("INSERT INTO SITE_BRUSH_TASK DEFAULT VALUES"); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT ID FROM SITE_BRUSH_TASK", "60")
}

func TestHistoricalTypesInvalidCounterRollsBackBothTables(t *testing.T) {
	for _, bad := range []string{"unknown", "9223372036854775808", "3.7"} {
		t.Run(bad, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "user.db")
			db := userSchemaFixture(t, path, `CREATE TABLE CUSTOM_WORDS("OFFSET" INTEGER); INSERT INTO CUSTOM_WORDS VALUES(12);
			CREATE TABLE SITE_BRUSH_TASK(ID INTEGER PRIMARY KEY,UPLOAD_SIZE TEXT);`)
			if _, err := db.Exec("INSERT INTO SITE_BRUSH_TASK VALUES(1,?)", bad); err != nil {
				t.Fatal(err)
			}
			if err := PrepareNativeUserDatabase(path); err == nil || !strings.Contains(err.Error(), "UPLOAD_SIZE") {
				t.Fatal(err)
			}
			requireSchemaValue(t, db, `SELECT typeof("OFFSET") FROM CUSTOM_WORDS`, "integer")
			requireSchemaValue(t, db, "SELECT UPLOAD_SIZE FROM SITE_BRUSH_TASK", bad)
			requireSchemaValue(t, db, "SELECT count(*) FROM pragma_table_info('SITE_BRUSH_TASK')", "2")
			requireSchemaValue(t, db, "SELECT count(*) FROM sqlite_schema WHERE name LIKE 'nastool_type_%'", "0")
			if _, err := db.Exec("UPDATE SITE_BRUSH_TASK SET UPLOAD_SIZE='42'"); err != nil {
				t.Fatal(err)
			}
			if err := PrepareNativeUserDatabase(path); err != nil {
				t.Fatal(err)
			}
			requireSchemaValue(t, db, "SELECT typeof(UPLOAD_SIZE) FROM SITE_BRUSH_TASK", "integer")
		})
	}
}

func TestHistoricalTypesRejectPrimaryKeyConversionThatWouldChangeIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db := userSchemaFixture(t, path, `
		CREATE TABLE SITE_BRUSH_TASK(DOWNLOAD_COUNT TEXT PRIMARY KEY);
		INSERT INTO SITE_BRUSH_TASK(rowid,DOWNLOAD_COUNT) VALUES(7,'42');`)
	if err := PrepareNativeUserDatabase(path); err == nil {
		t.Fatal("rowid and primary key cannot both be retained")
	}
	requireSchemaValue(t, db, "SELECT rowid||':'||typeof(DOWNLOAD_COUNT)||':'||DOWNLOAD_COUNT FROM SITE_BRUSH_TASK", "7:text:42")
}
