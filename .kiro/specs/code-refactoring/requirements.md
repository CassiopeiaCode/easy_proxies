# Requirements Document

## Introduction

本文档定义了 easy_proxies 项目代码重构的需求。通过分析现有代码，识别出逻辑复杂、反直觉或混乱的部分，并制定重构计划以提高代码可维护性、可读性和可测试性。

## Glossary

- **Pool**: 代理池模块，负责管理多个上游代理节点的选择、健康检查和故障处理
- **Member**: 代理池中的单个代理节点
- **Blacklist**: 黑名单机制，用于临时禁用故障节点
- **Health_Check**: 健康检查，用于验证代理节点的可用性
- **Monitor**: 监控模块，负责节点状态追踪和 Web UI 展示
- **State_Store**: 状态持久化存储，用于保存节点状态到磁盘
- **Entry_Handle**: 监控条目句柄，用于操作单个节点的监控状态

## Requirements

### Requirement 1: 拆分 pool.go 巨型文件

**User Story:** As a developer, I want the pool module to be split into smaller, focused files, so that I can understand and maintain the code more easily.

#### Acceptance Criteria

1. THE Refactoring SHALL split pool.go (1366 lines) into multiple files with clear responsibilities
2. WHEN splitting files, THE Refactoring SHALL create separate files for: connection tracking, health checking, member selection, and HTTP probing
3. THE Refactoring SHALL ensure each new file has less than 400 lines of code
4. THE Refactoring SHALL maintain all existing functionality without behavioral changes

### Requirement 2: 简化双重初始化逻辑

**User Story:** As a developer, I want the node registration logic to be simplified, so that I can understand when and how nodes are initialized.

#### Acceptance Criteria

1. THE Refactoring SHALL consolidate the duplicate node registration in newPool() and initializeMembersLocked()
2. WHEN a node is registered, THE System SHALL register it exactly once
3. THE Refactoring SHALL remove the confusing "register early, connect later" pattern
4. THE Refactoring SHALL document the initialization lifecycle clearly

### Requirement 3: 统一黑名单状态管理

**User Story:** As a developer, I want blacklist state to be managed in a single location, so that I can avoid state synchronization bugs.

#### Acceptance Criteria

1. THE Refactoring SHALL consolidate blacklist state between memberState, EntryHandle, and State_Store
2. WHEN blacklist state changes, THE System SHALL update all related components atomically
3. THE Refactoring SHALL create a single source of truth for blacklist status
4. IF blacklist state is queried, THEN THE System SHALL return consistent results regardless of query path

### Requirement 4: 提取健康检查为独立模块

**User Story:** As a developer, I want health check logic to be in a separate module, so that I can test and modify it independently.

#### Acceptance Criteria

1. THE Refactoring SHALL extract health check logic from pool.go into a dedicated health_checker module
2. THE Health_Checker module SHALL expose a clean interface for TCP and HTTP probing
3. THE Health_Checker module SHALL be independently testable
4. WHEN health check configuration changes, THE Health_Checker SHALL adapt without modifying pool logic

### Requirement 5: 简化 Monitor Manager 的职责

**User Story:** As a developer, I want the Monitor Manager to have a single, clear responsibility, so that I can understand its role in the system.

#### Acceptance Criteria

1. THE Refactoring SHALL separate node state tracking from probe execution in Monitor_Manager
2. THE Monitor_Manager SHALL only be responsible for state aggregation and API exposure
3. WHEN probe functions are needed, THE System SHALL delegate to the Health_Checker module
4. THE Refactoring SHALL remove circular dependencies between pool and monitor packages

### Requirement 6: 重构连接追踪机制

**User Story:** As a developer, I want connection tracking to be clearer and more maintainable, so that I can debug connection issues easily.

#### Acceptance Criteria

1. THE Refactoring SHALL extract trackedConn and trackedPacketConn into a separate connections module
2. THE Connections module SHALL provide clear lifecycle management (create, track, release)
3. WHEN a connection is closed, THE System SHALL guarantee cleanup happens exactly once
4. THE Refactoring SHALL simplify the "first byte timeout" and "minimum bytes" validation logic

### Requirement 7: 统一配置验证和默认值处理

**User Story:** As a developer, I want configuration handling to be consistent and predictable, so that I can understand what values are used at runtime.

#### Acceptance Criteria

1. THE Refactoring SHALL consolidate default value assignment between config.go, pool.go, and builder.go
2. WHEN a configuration value is not provided, THE System SHALL apply defaults in exactly one location
3. THE Refactoring SHALL create a validation layer that runs before any module uses the config
4. THE Refactoring SHALL document all default values in a single location

### Requirement 8: 简化 Builder 模块的错误处理

**User Story:** As a developer, I want build errors to be handled consistently, so that I can understand why nodes fail to build.

#### Acceptance Criteria

1. THE Refactoring SHALL create a structured error type for build failures
2. WHEN a node fails to build, THE System SHALL capture the failure reason, node index, and URI
3. THE Refactoring SHALL remove the regex-based error parsing (outboundIndexPattern)
4. THE Refactoring SHALL provide clear error messages without requiring log analysis

### Requirement 9: 提取协议解析为独立模块

**User Story:** As a developer, I want protocol parsing to be in a separate module, so that I can add new protocols without modifying the builder.

#### Acceptance Criteria

1. THE Refactoring SHALL extract buildVLESSOptions, buildVMessOptions, buildShadowsocksOptions, etc. into a protocols module
2. THE Protocols module SHALL use a registry pattern for protocol handlers
3. WHEN a new protocol is added, THE System SHALL only require adding a new handler to the registry
4. THE Refactoring SHALL ensure each protocol parser is independently testable

### Requirement 10: 改进并发控制的可理解性

**User Story:** As a developer, I want concurrency control to be explicit and documented, so that I can avoid race conditions.

#### Acceptance Criteria

1. THE Refactoring SHALL document all mutex usage with clear comments explaining what they protect
2. THE Refactoring SHALL reduce the number of separate locks where possible
3. WHEN multiple locks are needed, THE System SHALL document the lock ordering to prevent deadlocks
4. THE Refactoring SHALL consider using sync.Map or atomic operations where appropriate to simplify locking

