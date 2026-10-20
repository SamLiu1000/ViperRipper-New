@echo off
setlocal
chcp 65001 >nul 2>nul
cd /d "%~dp0"

echo ========================================
echo   ViperRipper New - Dev Launcher
echo ========================================
echo.

where wails >nul 2>nul
if errorlevel 1 (
    echo [ERROR] Wails CLI not found. Install it with:
    echo   go install github.com/wailsapp/wails/v2/cmd/wails@latest
    echo.
    pause
    exit /b 1
)

if not exist "go.mod" (
    echo [ERROR] go.mod not found. Run this script from the project root.
    pause
    exit /b 1
)

set "GOPROXY=https://goproxy.cn,direct"

echo [1/2] Checking Go dependencies...
go mod tidy
if errorlevel 1 (
    echo [ERROR] go mod tidy failed
    pause
    exit /b 1
)

echo.
echo [2/2] Starting Wails dev mode...
echo      Close this window to stop the app.
echo.

wails dev

if errorlevel 1 (
    echo.
    echo [ERROR] Wails dev failed
    pause
    exit /b 1
)

endlocal
