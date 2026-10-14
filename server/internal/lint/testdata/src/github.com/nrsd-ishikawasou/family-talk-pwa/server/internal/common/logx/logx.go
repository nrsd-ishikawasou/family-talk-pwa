// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package logx は試験の材料（本物の logx の型だけを写す）。
package logx

type LabelName string

type Logger struct{}

func (l *Logger) Info(msg string, fields ...any)  {}
func (l *Logger) Warn(msg string, fields ...any)  {}
func (l *Logger) Error(msg string, fields ...any) {}
func (l *Logger) Debug(msg string, fields ...any) {}
