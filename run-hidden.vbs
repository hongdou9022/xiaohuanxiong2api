Set fso = CreateObject("Scripting.FileSystemObject")
folder = fso.GetParentFolderName(WScript.ScriptFullName)
Set sh = CreateObject("WScript.Shell")
sh.Run "powershell -WindowStyle Hidden -ExecutionPolicy Bypass -File """ & folder & "\start-xhx2api.ps1""", 0, False