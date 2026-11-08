-- 0014_plugin_build: which plugin build a session loaded. The conventions
-- are 0001's.
--
-- A session loads the plugin's mod once, when it starts, and keeps it
-- until it restarts, so a session started before a `git pull` runs the
-- old mod. The mod reports, on session_start, the build it loaded
-- (internal/events' PluginBuild: `git describe --tags --always` of the
-- last commit that changed the plugin's folder, "-dirty" when its tracked
-- files differ) and the folder it loaded it from (PluginRoot), so the
-- dashboard can compare it with the build on disk there now. '' means not
-- reported: a mod older than this, or a folder that isn't a git checkout.
ALTER TABLE sessions ADD COLUMN plugin_build TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN plugin_root TEXT NOT NULL DEFAULT '';
