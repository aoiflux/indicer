## Quick Start Guide to DUES TUI

### Launch the Interactive Interface
```bash
dues tui
```

### Main Menu
When you launch the TUI, you'll see the main menu with these options:

1. **💾 Store File** - Add a new file to the DUES database
2. **📂 List Files** - View all files currently stored
3. **🔍 Search** - Search for content in the database
4. **🔄 Restore File** - Extract a file from the database
5. **🔎 NeAR Analysis** - Find similar files
6. **🗑️ Reset Database** - Delete the entire database (⚠️ WARNING!)

### Navigation
- Use **↑/↓ arrow keys** to move between menu items
- Press **Enter** to select an option
- Press **ESC** to return to the main menu
- Press **q** to quit (from main menu only)
- Press **ctrl+c** to force exit

### Working with Files

#### Storing a File
1. Select "💾 Store File"
2. Enter the path to your file
3. Choose options:
   - **Sync Index** - Index synchronously (blocks dedup)
   - **Skip Index** - Don't create index
4. Press **Tab** to navigate, **Enter** to confirm

#### Listing Files
1. Select "📂 List Files"
2. View all stored files with their hashes and chunk counts
3. Press ESC to return

#### Searching
1. Select "🔍 Search"
2. Enter your search query
3. Results appear below

#### Restoring Files
1. Select "🔄 Restore File"
2. Enter the file hash (from List Files)
3. Enter where to save the file (default: "restored")
4. Confirm to restore

#### NeAR Analysis
1. Select "🔎 NeAR Analysis"
2. Choose mode:
   - **📂 Find in Database** - Analyze files already stored
   - **📁 Find from File** - Compare external file against DB
3. Enable deep scan if needed (slower but finds partial matches)
4. Confirm to analyze

### Tips & Tricks

**🔐 Password Management**
- Set password on first launch: `dues tui --password mysecret`
- Database stays encrypted with your password

**⚡ Performance Options**
```bash
# Fast mode (skip encryption/compression)
dues tui --quick

# Low resource mode (reduces CPU usage)
dues tui --low

# Container mode (good for large databases)
dues tui --container
```

**📊 Custom Chunk Size**
```bash
dues tui --chonksize 512  # Use 512KB chunks instead of 256KB
```

**🗃️ Custom Database Location**
```bash
dues tui --dbpath /mnt/storage/evidence
```

### Combining Options
```bash
dues tui --dbpath ./my-db --password secure123 --chonksize 512 --container
```

### Troubleshooting

**"Error: Database not found"**
- Ensure the path exists or use `--dbpath` to specify location
- First use will create the database

**"Wrong password"**
- The database password cannot be changed
- Ensure you're using the same password as before

**"Terminal too small"**
- Expand your terminal window
- Minimum recommended: 80 columns × 20 rows

**Non-Latin characters not displaying**
- Ensure your terminal supports UTF-8
- Most modern terminals do this by default

---

For detailed commands and options, see the main README or run:
```bash
dues help tui
```
