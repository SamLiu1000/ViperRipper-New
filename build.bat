@echo off
setlocal
chcp 65001 >nul 2>nul
cd /d "%~dp0"

echo ========================================
echo   ViperRipper New - Build Script
echo ========================================
echo.

where go >nul 2>nul
if errorlevel 1 (
    echo [ERROR] Go not found in PATH.
    pause
    exit /b 1
)

where wails >nul 2>nul
if errorlevel 1 (
    echo [ERROR] Wails CLI not found. Install it with:
    echo   go install github.com/wailsapp/wails/v2/cmd/wails@latest
    echo.
    pause
    exit /b 1
)

set "GOPROXY=https://goproxy.cn,direct"

echo [1/3] go mod tidy...
go mod tidy
if errorlevel 1 (
    echo [ERROR] go mod tidy failed
    pause
    exit /b 1
)

echo.
echo [2/3] wails build...
wails build -ldflags="-w -s" -trimpath
if errorlevel 1 (
    echo [ERROR] Build failed
    pause
    exit /b 1
)

echo.
echo [3/3] Preparing portable output...
if not exist "build\bin\data\hosts" (
    echo       data dir will be created on first run
)

echo.
echo ========================================
echo   Build succeeded!
echo   Output: %~dp0build\bin\ViperRipper New.exe
echo   Portable: copy "ViperRipper New.exe" + data folder together.
echo ========================================
echo.

endlocal
pause
