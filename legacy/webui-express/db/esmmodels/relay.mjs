import { Model, DataTypes } from "../../src/adapter.js";
import sequelize from "../config/sequelize.mjs";

export class Relay extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
        // define association here
    }
}

Relay.init(
    {
        group_id: DataTypes.INTEGER,
        name: DataTypes.STRING,
        host: DataTypes.STRING,
        port: DataTypes.INTEGER,
        auth_user: DataTypes.STRING,
        auth_pass: DataTypes.STRING,
        priority: DataTypes.INTEGER,
    },
    {
        sequelize,
        modelName: "Relay",
    }
);

export default Relay;
