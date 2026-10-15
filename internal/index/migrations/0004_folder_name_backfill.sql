-- Copies recorded before 0003 have no display name; use the raw name.
UPDATE server_copies SET folder_name = folder WHERE folder_name = '';
