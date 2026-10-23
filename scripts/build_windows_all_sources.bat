@echo off
chcp 65001 >nul
setlocal
cd /d "%~dp0.."
set PYTHONIOENCODING=utf-8

echo ================================================
echo  建置 Windows 全片源版（真果鉴）
echo  目录：%CD%
echo ================================================
echo.

set MISSING=
for %%T in (py flutter go gcc nuget) do (
  where %%T >nul 2>nul || call set "MISSING=%%MISSING%% %%T"
)
if defined MISSING (
  echo [错误] 找不到以下工具，请先安装并加入 PATH：%MISSING%
  goto :failed
)

py scripts\build_windows.py --all-sources %*
if errorlevel 1 goto :failed

echo.
echo ================================================
echo  建置完成，产物位于 dist\windows
echo ================================================
if exist "dist\windows" start "" explorer "dist\windows"
pause
exit /b 0

:failed
echo.
echo ================================================
echo  建置失败，请查看上方的错误信息
echo ================================================
pause
exit /b 1
